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
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/transport"
)

const ScopeHeader = "X-Chrysopoeia-Proxy-Scope"
const ScopeAnnotation = "proxy.chrysopoeia.io/scope"

const internalErrorHeader = "X-Chrysopoeia-Proxy-Error"
const internalErrorStatusCodeHeader = "X-Chrysopoeia-Proxy-Error-Status-Code"
const scopedListVerb = "scopedlist"

// New creates a new HTTP handler that proxies requests to the upstream Kubernetes API server,
// rewriting requests and responses as necessary to enforce access control based on the scope label.
// Shutting down the context will stop the internal service account cache.
func New(ctx context.Context, upstreamRestConf *rest.Config) (http.Handler, error) {
	flag.Parse()

	scheme, decoder, err := newDecoder()
	if err != nil {
		return nil, err
	}
	encoder := newEncoder(scheme)

	if !impersonationEmpty(upstreamRestConf) {
		return nil, fmt.Errorf("impersonation is not supported for the upstream config")
	}

	kubeClient, err := kubernetes.NewForConfig(upstreamRestConf)
	if err != nil {
		return nil, err
	}

	authenticationClient := kubeClient.AuthenticationV1()
	authorizationClient := kubeClient.AuthorizationV1()

	scopeExtractor := newScopeExtractor(ctx, kubeClient)

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

	rp := &httputil.ReverseProxy{
		Transport: &internalErrorRoundtripper{parent: upstreamTransport},
		Rewrite: rewriteErrorWrapper(func(r *httputil.ProxyRequest) error {
			r.SetXForwarded()
			r.SetURL(upstreamURL)

			requestInfo, err := decodeRequestInfo(r.In)
			if err != nil {
				return fmt.Errorf("failed to decode request info: %w", err)
			}

			if !requestNeedsRewrite(requestInfo) {
				return nil
			}

			userInfo, err := extractUserInfoFromAuthHeader(r.In.Context(), authenticationClient, r.In.Header.Get("Authorization"))
			if err != nil {
				return fmt.Errorf("failed to extract user info from bearer token: %w", err)
			}

			scopeLabel, err := scopeExtractor.getScopeLabel(r.In.Header, userInfo.Username)
			if err != nil {
				return fmt.Errorf("failed to get scope label: %w", err)
			}

			log.Printf("Rewriting request. Scope: %q, RequestInfo: %+v, UserInfo: %+v", scopeLabel, requestInfo, userInfo)

			allowed, reason, err := checkCustomVerbAccess(r.In.Context(), authorizationClient, requestInfo.APIGroup, requestInfo.APIVersion, requestInfo.Resource, userInfo, scopeLabel)
			if err != nil {
				return fmt.Errorf("failed to check custom verb access: %w", err)
			}
			if !allowed {
				return &errorWithStatusCode{
					error:      fmt.Errorf("user %s is not allowed to list cluster-scoped resources with label %s: %s", userInfo.Username, scopeLabel, reason),
					statusCode: http.StatusForbidden,
				}
			}

			// Use our authentication that has cluster scoped list/watch.
			r.Out.Header.Del("Authorization")

			// Validates the label so users can't just list everything by setting the selector to something like `!notexists`.
			// Note we authenticate the label but it's somewhat easy to misconfigure and allow any label.
			req, err := labels.NewRequirement(scopeLabel, selection.Exists, nil)
			if err != nil {
				return &errorWithStatusCode{
					error:      fmt.Errorf("failed to create label requirement: %w", err),
					statusCode: http.StatusUnprocessableEntity,
				}
			}
			ls := req.String()

			q := r.Out.URL.Query()
			if existing := q.Get("labelSelector"); existing != "" {
				ls = strings.Join([]string{existing, ls}, ",")
			}
			q.Set("labelSelector", ls)
			r.Out.URL.RawQuery = q.Encode()

			return nil
		}),

		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			statusCode := http.StatusInternalServerError
			if esc, ok := errors.AsType[*errorWithStatusCode](err); ok {
				statusCode = esc.statusCode
			}
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.WriteHeader(statusCode)
			_, _ = rw.Write([]byte(fmt.Sprintf("Proxy error: %s", err.Error())))
		},

		ModifyResponse: func(res *http.Response) error {
			if res.Request.Method == http.MethodConnect {
				return nil
			}

			requestInfo, err := decodeRequestInfo(res.Request)
			if err != nil {
				return fmt.Errorf("failed to get request info: %w", err)
			}

			if requestInfo.Verb != "create" ||
				requestInfo.APIGroup != authorizationv1.SchemeGroupVersion.Group ||
				requestInfo.APIVersion != authorizationv1.SchemeGroupVersion.Version ||
				requestInfo.Resource != "selfsubjectaccessreviews" {
				return nil
			}

			log.Printf("Modifying SelfSubjectAccessReview response for request: %+v", requestInfo)

			userInfo, err := extractUserInfoFromAuthHeader(res.Request.Context(), authenticationClient, res.Request.Header.Get("Authorization"))
			if err != nil {
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
					scopeLabel, err := scopeExtractor.getScopeLabel(res.Request.Header, userInfo.Username)
					if err != nil {
						return fmt.Errorf("failed to get scope label: %w", err)
					}

					allowed, reason, err := checkCustomVerbAccess(res.Request.Context(), authorizationClient, obj.Spec.ResourceAttributes.Group, obj.Spec.ResourceAttributes.Version, obj.Spec.ResourceAttributes.Resource, userInfo, scopeLabel)
					if err != nil {
						return fmt.Errorf("failed to check custom verb access: %w", err)
					}
					if allowed {
						log.Printf("Allowing cluster-scoped access through proxy for resource %q/%q with verb %q\n", obj.Spec.ResourceAttributes.Group, obj.Spec.ResourceAttributes.Resource, obj.Spec.ResourceAttributes.Verb)
					}
					obj.Status.Allowed = allowed
					obj.Status.Denied = !allowed
					obj.Status.Reason = fmt.Sprintf("Access managed through chrysopoeia.io proxy for cluster-scoped resources: %s (Upstream reason: %s)", reason, obj.Status.Reason)
				}
				decodedObj = obj
			default:
				return fmt.Errorf("unexpected response type for SelfSubjectAccessReview: %T", obj)
			}

			if err := encoder.encodeIntoBody(res, decodedObj); err != nil {
				return fmt.Errorf("failed to encode modified response body: %w", err)
			}

			return nil
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_readyz", func(w http.ResponseWriter, r *http.Request) {
		if !scopeExtractor.ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/_healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", rp)

	return mux, nil
}

var requestInfoFactory = new(request.RequestInfoFactory{
	APIPrefixes: sets.NewString(
		strings.Trim(server.APIGroupPrefix, "/"),
		strings.Trim(server.DefaultLegacyAPIPrefix, "/"),
	),
	GrouplessAPIPrefixes: sets.NewString(
		strings.Trim(server.DefaultLegacyAPIPrefix, "/"),
	),
})

func decodeRequestInfo(req *http.Request) (request.RequestInfo, error) {
	ri, err := requestInfoFactory.NewRequestInfo(req)
	if err != nil {
		return request.RequestInfo{}, err
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

type internalErrorRoundtripper struct {
	parent http.RoundTripper
}

func (rtf *internalErrorRoundtripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if errStr := r.Header.Get(internalErrorHeader); errStr != "" {
		if r.Body != nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		if code := r.Header.Get(internalErrorStatusCodeHeader); code != "" {
			statusCode, err := strconv.Atoi(code)
			if err != nil {
				return nil, fmt.Errorf("failed to parse internal error status code: %w", err)
			}
			return nil, &errorWithStatusCode{
				error:      errors.New(errStr),
				statusCode: statusCode,
			}
		}
		return nil, errors.New(errStr)
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

func checkCustomVerbAccess(ctx context.Context, authClient authorizationv1client.AuthorizationV1Interface, group, version, resource string, userInfo authenticationv1.UserInfo, injectedLabel string) (bool, string, error) {
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
				Group:    group,
				Version:  version,
				Resource: resource,
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

type scopeExtractor struct {
	saStore cache.Store
}

// newScopeExtractor creates a new scopeExtractor that maintains a cache of service accounts in the cluster.
// The cache is updated in the background and can be stopped by canceling the provided context.
func newScopeExtractor(ctx context.Context, coreClient kubernetes.Interface) *scopeExtractor {
	saStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	go cache.NewReflector(
		cache.NewListWatchFromClient(coreClient.CoreV1().RESTClient(), "serviceaccounts", metav1.NamespaceAll, fields.Everything()),
		&corev1.ServiceAccount{},
		saStore,
		time.Hour,
	).RunWithContext(ctx)

	return &scopeExtractor{saStore: saStore}
}

func (se *scopeExtractor) ready() bool {
	return se.saStore.LastStoreSyncResourceVersion() != ""
}

// getScopeLabel extracts the scope label from the request header or from the service account annotation if the user is a service account.
func (se *scopeExtractor) getScopeLabel(header http.Header, username string) (string, error) {
	var injectedLabel string
	// Since we authenticate the user and check their access to the injected label,
	// we can trust the header.
	if lbl := header.Get(ScopeHeader); lbl != "" {
		injectedLabel = lbl
	} else if strings.HasPrefix(username, "system:serviceaccount:") {
		// If the user is a service account, we can get the scope from an annotation on the service account. This is a fallback for when the request doesn't have the scope header.
		ns, name, ok := strings.Cut(strings.TrimPrefix(username, "system:serviceaccount:"), ":")
		if !ok {
			return "", fmt.Errorf("failed to parse service account username, missing ':' in %q", username)
		}
		obj, exists, err := se.saStore.GetByKey(ns + "/" + name)
		if err != nil {
			return "", fmt.Errorf("failed to get service account %s/%s from store: %v", ns, name, err)
		}
		if !exists {
			return "", fmt.Errorf("service account %s/%s not found in store", ns, name)
		}
		sa := obj.(*corev1.ServiceAccount)
		injectedLabel = sa.Annotations[ScopeAnnotation]
	}

	if injectedLabel == "" {
		return "", fmt.Errorf("failed to determine injected label for user %q, either set the %q header or annotate the service account with %q", username, ScopeHeader, ScopeAnnotation)
	}
	return injectedLabel, nil
}

// rewriteErrorWrapper wraps a function that modifies a ProxyRequest sets the error header if the function returns an error.
// This is a workaround for the fact that the ReverseProxy does not have a way to return an error from the Rewrite function.
// Must be paired with the internalErrorRoundtripper to work correctly.
func rewriteErrorWrapper(f func(*httputil.ProxyRequest) error) func(r *httputil.ProxyRequest) {
	return func(r *httputil.ProxyRequest) {
		r.Out.Header.Del(internalErrorHeader)
		r.Out.Header.Del(internalErrorStatusCodeHeader)
		if err := f(r); err != nil {
			r.Out.Header.Set(internalErrorHeader, err.Error())
			if esc, ok := errors.AsType[*errorWithStatusCode](err); ok {
				r.Out.Header.Set(internalErrorStatusCodeHeader, strconv.Itoa(esc.statusCode))
			}
		}
	}
}

type errorWithStatusCode struct {
	error
	statusCode int
}

func (e *errorWithStatusCode) Error() string {
	return fmt.Sprintf("%s (status code: %d)", e.error.Error(), e.statusCode)
}

func newEncoder(scheme *runtime.Scheme) *encoder {
	return &encoder{
		protoSerializer: protobuf.NewSerializer(scheme, scheme),
		jsonSerializer:  json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{}),
		yamlSerializer:  json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{Yaml: true}),
		cborSerializer:  cbor.NewSerializer(scheme, scheme),
	}
}

type encoder struct {
	protoSerializer *protobuf.Serializer
	jsonSerializer  *json.Serializer
	yamlSerializer  *json.Serializer
	cborSerializer  cbor.Serializer
}

func (e *encoder) encoderForMediaType(mediaType string) (runtime.Encoder, error) {
	mediaType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return nil, fmt.Errorf("failed to parse media type: %w", err)
	}

	// https://kubernetes.io/docs/reference/using-api/api-concepts/#alternate-representations-of-resources
	var enc runtime.Encoder
	switch mediaType {
	case "application/json":
		enc = e.jsonSerializer
	case "application/yaml":
		enc = e.yamlSerializer
	case "application/vnd.kubernetes.protobuf":
		enc = e.protoSerializer
	case "application/cbor":
		enc = e.cborSerializer
	default:
		return nil, fmt.Errorf("unsupported media type: %q", mediaType)
	}

	return enc, nil
}

func (e *encoder) encodeIntoBody(res *http.Response, obj runtime.Object) error {
	contentType := res.Header.Get("Content-Type")
	enc, err := e.encoderForMediaType(contentType)
	if err != nil {
		return fmt.Errorf("failed to get encoder for Content-Type %q: %w", contentType, err)
	}

	buf := &bytes.Buffer{}
	if err := enc.Encode(obj, buf); err != nil {
		return fmt.Errorf("failed to encode %s: %w", contentType, err)
	}

	res.Header.Del("Content-Length")
	res.Body = io.NopCloser(buf)
	res.ContentLength = int64(buf.Len())

	return nil
}
