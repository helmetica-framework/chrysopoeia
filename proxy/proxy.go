package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/transport"
	ctrl "sigs.k8s.io/controller-runtime"
)

const labelSelector = "requires.helmetica.io/mariadbs.k8s.mariadb.com"

func main() {
	scheme, decoder, err := newDecoder()
	if err != nil {
		log.Fatal(err)
	}
	protoEncoder := protobuf.NewSerializer(scheme, scheme)

	upstreamRestConf := ctrl.GetConfigOrDie()

	upstreamTransportConf, err := upstreamRestConf.TransportConfig()
	if err != nil {
		log.Fatal(err)
	}
	upstreamTransport, err := transport.New(upstreamTransportConf)
	if err != nil {
		log.Fatal(err)
	}

	upstreamURL, err := url.Parse(upstreamRestConf.Host)
	if err != nil {
		log.Fatal(err)
	}
	frontendProxy := httptest.NewServer(&httputil.ReverseProxy{
		Transport: upstreamTransport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetXForwarded()
			r.SetURL(upstreamURL)

			requestInfo, err := decodeRequestInfo(r.In)
			if err != nil {
				log.Printf("Failed to get request info: %v", err)
				return
			}
			log.Printf("RequestInfo: %+v", requestInfo)

			if requestInfo.Namespace != "" {
				return
			}

			ls := labelSelector
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
			log.Printf("ModifyResponse RequestInfo: %+v", requestInfo)

			if requestInfo.Verb != "create" ||
				requestInfo.APIGroup != authorizationv1.SchemeGroupVersion.Group ||
				requestInfo.APIVersion != authorizationv1.SchemeGroupVersion.Version ||
				requestInfo.Resource != "selfsubjectaccessreviews" {
				return nil
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
	})
	defer frontendProxy.Close()

	log.Printf("Proxy server listening on %s", frontendProxy.URL)

	select {}
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
