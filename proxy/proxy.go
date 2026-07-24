package proxy

import (
	"bytes"
	"context"
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
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

const internalErrorHeader = "X-Chrysopoeia-Proxy-Error"
const scopedListVerb = "scopedlist"

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
	upstreamAuthorizationClient, err := authorizationv1client.NewForConfig(upstreamRestConf)
	if err != nil {
		return nil, err
	}

	upstreamTransportConf, err := upstreamRestConf.TransportConfig()
	if err != nil {
		return nil, err
	}
	if upstreamTransportConf.HasCertAuth() || upstreamTransportConf.HasCertCallback() {
		return nil, fmt.Errorf("client certificate authentication is not supported for the upstream config")
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

			if !requestNeedsRewrite(requestInfo) {
				return
			}

			userInfo, err := extractUserInfoFromAuthHeader(r.In.Context(), upstreamAuthenticationClient, r.In.Header.Get("Authorization"))
			if err != nil {
				log.Printf("Failed to extract user info from bearer token: %v", err)
				r.Out.Header.Set(internalErrorHeader, fmt.Sprintf("Failed to extract user info from bearer token: %v", err))
				return
			}

			log.Printf("Rewriting request. RequestInfo: %+v, UserInfo: %+v", requestInfo, userInfo)

			allowed, reason, err := checkCustomVerbAccess(r.In.Context(), upstreamAuthorizationClient, requestInfo, userInfo, injectedLabel)
			if err != nil {
				log.Printf("Failed to check custom verb access: %v", err)
				r.Out.Header.Set(internalErrorHeader, fmt.Sprintf("Failed to check custom verb access: %v", err))
				return
			}
			if !allowed {
				log.Printf("User %s is not allowed to list cluster-scoped resources with label %s: %s", userInfo.Username, injectedLabel, reason)
				r.Out.Header.Set(internalErrorHeader, fmt.Sprintf("User %s is not allowed to list cluster-scoped resources with label %s: %s", userInfo.Username, injectedLabel, reason))
				return
			}

			// Use our authentication that has cluster scoped list/watch.
			r.Out.Header.Del("Authorization")

			ls := injectedLabel
			q := r.Out.URL.Query()
			if existing := q.Get("labelSelector"); existing != "" {
				ls = strings.Join([]string{existing, ls}, ",")
			}
			q.Set("labelSelector", ls)
			r.Out.URL.RawQuery = q.Encode()
		},

		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.WriteHeader(http.StatusInternalServerError)
			_, _ = rw.Write([]byte(fmt.Sprintf("Proxy error: %s", err.Error())))
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

			if requestInfo.Verb != "create" ||
				requestInfo.APIGroup != authorizationv1.SchemeGroupVersion.Group ||
				requestInfo.APIVersion != authorizationv1.SchemeGroupVersion.Version ||
				requestInfo.Resource != "selfsubjectaccessreviews" {
				return nil
			}

			log.Printf("Modifying SelfSubjectAccessReview response for request: %+v", requestInfo)

			userInfo, err := extractUserInfoFromAuthHeader(res.Request.Context(), upstreamAuthenticationClient, res.Request.Header.Get("Authorization"))
			if err != nil {
				log.Printf("Failed to extract user info from bearer token: %v", err)
				res.Header.Set(internalErrorHeader, fmt.Sprintf("Failed to extract user info from bearer token: %v", err))
				return fmt.Errorf("failed to extract user info from request header: %w", err)
			}

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
				if !obj.Status.Allowed &&
					obj.Spec.ResourceAttributes.Namespace == "" &&
					matchesListWatchVerb(obj.Spec.ResourceAttributes.Verb) {
					log.Printf("Allowing cluster-scoped access through proxy for resource %s/%s with verb %s\n", obj.Spec.ResourceAttributes.Group, obj.Spec.ResourceAttributes.Resource, obj.Spec.ResourceAttributes.Verb)

					allowed, reason, err := checkCustomVerbAccess(res.Request.Context(), upstreamAuthorizationClient, requestInfo, userInfo, injectedLabel)
					if err != nil {
						log.Printf("Failed to check custom verb access: %v", err)
						res.Header.Set(internalErrorHeader, fmt.Sprintf("Failed to check custom verb access: %v", err))
						return fmt.Errorf("failed to check custom verb access: %w", err)
					}
					obj.Status.Allowed = allowed
					obj.Status.Denied = !allowed
					obj.Status.Reason = fmt.Sprintf("Access managed through helmetica.io proxy for cluster-scoped resources: %s (Upstream reason: %s)", reason, obj.Status.Reason)
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

func decodeRequestInfo(req *http.Request) (request.RequestInfo, error) {
	ri, err := new(request.RequestInfoFactory{
		APIPrefixes: sets.NewString(
			strings.Trim(server.APIGroupPrefix, "/"),
			strings.Trim(server.DefaultLegacyAPIPrefix, "/"),
		),
		GrouplessAPIPrefixes: sets.NewString(
			strings.Trim(server.DefaultLegacyAPIPrefix, "/"),
		),
	}).NewRequestInfo(req)
	if err != nil {
		return request.RequestInfo{}, err
	}
	if ri == nil {
		return request.RequestInfo{}, errors.New("request info is nil")
	}
	return *ri, nil
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

func extractUserInfoFromAuthHeader(ctx context.Context, upstreamAuthenticationClient authenticationv1client.AuthenticationV1Interface, authHeader string) (authenticationv1.UserInfo, error) {
	rawJWT := strings.TrimPrefix(authHeader, "Bearer ")
	if rawJWT == "" || strings.HasPrefix(rawJWT, "Basic ") {
		return authenticationv1.UserInfo{}, fmt.Errorf("only Bearer tokens are supported for authentication")
	}

	tokenReview, err := upstreamAuthenticationClient.TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token: rawJWT,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return authenticationv1.UserInfo{}, fmt.Errorf("failed to create TokenReview: %w", err)
	}
	if !tokenReview.Status.Authenticated {
		return authenticationv1.UserInfo{}, fmt.Errorf("token is not authenticated: %s", tokenReview.Status.Error)
	}
	return tokenReview.Status.User, nil
}

func checkCustomVerbAccess(ctx context.Context, authClient authorizationv1client.AuthorizationV1Interface, requestInfo request.RequestInfo, userInfo authenticationv1.UserInfo, injectedLabel string) (bool, string, error) {
	extra := make(map[string]authorizationv1.ExtraValue)
	for k, v := range userInfo.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	ssar, err := authClient.SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   userInfo.Username,
			UID:    userInfo.UID,
			Groups: userInfo.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     scopedListVerb,
				Group:    requestInfo.APIGroup,
				Version:  requestInfo.APIVersion,
				Resource: requestInfo.Resource,
				Name:     injectedLabel,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", fmt.Errorf("failed to create SubjectAccessReview: %w", err)
	}
	return ssar.Status.Allowed, ssar.Status.Reason, nil
}

func requestNeedsRewrite(requestInfo request.RequestInfo) bool {
	return requestInfo.IsResourceRequest &&
		requestInfo.Namespace == "" &&
		matchesListWatchVerb(requestInfo.Verb)
}

func matchesListWatchVerb(verb string) bool {
	return slices.ContainsFunc([]string{"list", "watch"}, func(v string) bool { return strings.EqualFold(v, verb) })
}
