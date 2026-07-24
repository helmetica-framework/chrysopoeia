package proxy

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"

	proxyrequest "github.com/helmetica-framework/chrysopoeia/proxy/request"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

const internalErrorHeader = "X-Chrysopoeia-Proxy-Error"

func New(upstreamRestConf *rest.Config, injectedLabel string) (http.Handler, error) {
	flag.Parse()

	scheme, decoder, err := newDecoder()
	if err != nil {
		return nil, err
	}
	protoEncoder := protobuf.NewSerializer(scheme, scheme)

	if !impersonationEmpty(upstreamRestConf) {
		return nil, fmt.Errorf("impersonation is not supported for the upstream config")
	}

	upstreamAuthenticationClient, err := authenticationv1client.NewForConfig(upstreamRestConf)
	if err != nil {
		return nil, err
	}

	upstreamTransportConf, err := upstreamRestConf.TransportConfig()
	if err != nil {
		return nil, err
	}
	upstreamTransport, err := transport.New(upstreamTransportConf)
	if err != nil {
		return nil, err
	}

	upstreamURL, err := url.Parse(upstreamRestConf.Host)
	if err != nil {
		return nil, err
	}

	return &httputil.ReverseProxy{
		Transport: &internalErrorRoundtripper{parent: upstreamTransport},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.Header.Del(internalErrorHeader)
			r.SetXForwarded()
			r.SetURL(upstreamURL)

			requestInfo, err := decodeRequestInfo(r.In)
			if err != nil {
				log.Printf("Failed to get request info: %v", err)
				return
			}

			rawJWT := strings.TrimPrefix(r.In.Header.Get("Authorization"), "Bearer ")
			if rawJWT == "" || strings.HasPrefix(rawJWT, "Basic ") {
				r.Out.Header.Set(internalErrorHeader, "Only Bearer tokens are supported for authentication")
				return
			}
			r.Out.Header.Del("Authorization")

			tokenReview, err := upstreamAuthenticationClient.TokenReviews().Create(r.In.Context(), &authenticationv1.TokenReview{
				Spec: authenticationv1.TokenReviewSpec{
					Token: rawJWT,
				},
			}, metav1.CreateOptions{})
			if err != nil {
				log.Printf("Failed to create TokenReview: %v", err)
				r.Out.Header.Set(internalErrorHeader, fmt.Sprintf("Failed to create TokenReview: %v", err))
				return
			}
			if !tokenReview.Status.Authenticated {
				log.Printf("Token is not authenticated: %v", tokenReview.Status.Error)
				r.Out.Header.Set(internalErrorHeader, fmt.Sprintf("Token is not authenticated: %v", tokenReview.Status.Error))
				return
			}
			userInfo := tokenReview.Status.User

			log.Printf("RequestInfo: %+v, UserInfo: %+v", requestInfo, userInfo)

			if requestInfo.Namespace != "" {
				// Pass request through without modification if it's namespaced, as we only want to modify cluster-scoped requests.
				proxyrequest.SetImpersonationHeaders(r.Out, &userInfo)
				return
			}

			ls := injectedLabel
			q := r.Out.URL.Query()
			if existing := q.Get("labelSelector"); existing != "" {
				ls = strings.Join([]string{existing, ls}, ",")
			}
			q.Set("labelSelector", ls)
			r.Out.URL.RawQuery = q.Encode()
		},

		ModifyResponse: func(res *http.Response) error {
			if res.Request.Method == http.MethodConnect {
				return nil
			}

			requestInfo, err := decodeRequestInfo(res.Request)
			if err != nil {
				log.Printf("Failed to get request info: %v", err)
				return err
			}

			log.Println("Headers", res.Request.Header, res.Header)

			if requestInfo.Verb != "create" ||
				requestInfo.APIGroup != authorizationv1.SchemeGroupVersion.Group ||
				requestInfo.APIVersion != authorizationv1.SchemeGroupVersion.Version ||
				requestInfo.Resource != "selfsubjectaccessreviews" {
				return nil
			}

			log.Printf("Modifying SelfSubjectAccessReview response for request: %+v", requestInfo)

			rawBody, err := io.ReadAll(res.Body)
			if err != nil {
				return fmt.Errorf("failed to read response body: %w", err)
			}
			res.Body.Close()

			decodedObj, _, err := decoder.Decode(rawBody, nil, nil)
			if err != nil {
				return fmt.Errorf("failed to decode response body: %w", err)
			}
			log.Printf("Decoded response body %T: %+v\n", decodedObj, decodedObj)

			switch obj := decodedObj.(type) {
			case *authorizationv1.SelfSubjectAccessReview:
				if !obj.Status.Allowed && obj.Spec.ResourceAttributes.Namespace == "" &&
					slices.ContainsFunc([]string{"list", "watch"}, func(verb string) bool { return strings.EqualFold(verb, obj.Spec.ResourceAttributes.Verb) }) {
					log.Printf("Allowing cluster-scoped access through proxy for resource %s/%s with verb %s\n", obj.Spec.ResourceAttributes.Group, obj.Spec.ResourceAttributes.Resource, obj.Spec.ResourceAttributes.Verb)
					obj.Status.Allowed = true
					obj.Status.Denied = false
					obj.Status.Reason = "Access allowed through helmetica.io proxy for cluster-scoped resources"
				}
				decodedObj = obj
			default:
				log.Printf("Unexpected object type: %T\n", obj)
			}

			var mediaType string
			if mediaType, _, err = mime.ParseMediaType(res.Request.Header.Get("Content-Type")); err != nil {
				log.Printf("Failed to parse media type: %v", err)
			}

			switch mediaType {
			case "application/json":
				if encoded, encodeErr := jsonEncode(decodedObj, scheme); encodeErr != nil {
					log.Printf("Failed to encode JSON: %v", encodeErr)
				} else {
					rawBody = encoded
				}
			case "application/vnd.kubernetes.protobuf":
				if encoded, encodeErr := runtime.Encode(protoEncoder, decodedObj); encodeErr != nil {
					log.Printf("Failed to encode protobuf: %v", encodeErr)
				} else {
					rawBody = encoded
				}
			}

			res.Header.Del("Content-Length")
			res.Body = io.NopCloser(bytes.NewReader(rawBody))
			res.ContentLength = int64(len(rawBody))

			return nil
		},
	}, nil
}

func decodeRequestInfo(req *http.Request) (*request.RequestInfo, error) {
	return new(request.RequestInfoFactory{
		APIPrefixes: sets.NewString(
			strings.Trim(server.APIGroupPrefix, "/"),
			strings.Trim(server.DefaultLegacyAPIPrefix, "/"),
		),
		GrouplessAPIPrefixes: sets.NewString(
			strings.Trim(server.DefaultLegacyAPIPrefix, "/"),
		),
	}).NewRequestInfo(req)
}

func newDecoder() (*runtime.Scheme, runtime.Decoder, error) {
	scheme := runtime.NewScheme()

	if err := authorizationv1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}

	codecFactory := serializer.NewCodecFactory(scheme)
	universalDecoder := codecFactory.UniversalDeserializer()
	return scheme, universalDecoder, nil
}

func jsonEncode(obj runtime.Object, scheme *runtime.Scheme) ([]byte, error) {
	return runtime.Encode(json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{}), obj)
}

type internalErrorRoundtripper struct {
	parent http.RoundTripper
}

func (rtf *internalErrorRoundtripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if err, ok := r.Header[internalErrorHeader]; ok {
		if r.Body != nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		return nil, errors.New(strings.Join(err, ","))
	}
	return rtf.parent.RoundTrip(r)
}

func impersonationEmpty(restConf *rest.Config) bool {
	return restConf.Impersonate.UserName == "" &&
		restConf.Impersonate.UID == "" &&
		len(restConf.Impersonate.Groups) == 0 &&
		len(restConf.Impersonate.Extra) == 0
}
