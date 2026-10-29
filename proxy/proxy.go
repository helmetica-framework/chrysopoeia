package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const ScopeHeader = "X-Chrysopoeia-Proxy-Scope"
const ScopeAnnotation = "proxy.chrysopoeia.io/scope"

const internalErrorHeader = "X-Chrysopoeia-Proxy-Error"
const internalErrorStatusCodeHeader = "X-Chrysopoeia-Proxy-Error-Status-Code"

// ScopedListVerb is the custom verb the proxy authorizes a scoped cluster-wide list or watch with.
// The harness grants it to the operator, with the scope label as the resource name.
const ScopedListVerb = "scopedlist"

type LabelInjection int

const (
	LabelInjectionNone LabelInjection = iota
	LabelInjectionClusterScoped
	LabelInjectionAll
)

// New creates a new HTTP handler that proxies requests to the upstream Kubernetes API server,
// rewriting requests and responses as necessary to enforce access control based on the scope label.
// Shutting down the context will stop the internal service account cache.
func New(ctx context.Context, upstreamRestConf *rest.Config, ljConfig LabelInjection, labelInjectorMaxRequestBodySize int64) (http.Handler, error) {
	l := log.FromContext(ctx).WithName("proxy")

	scheme, decoder, err := newDecoder()
	if err != nil {
		return nil, err
	}
	serializer := newContentSerializer(scheme)

	if !impersonationEmpty(upstreamRestConf) {
		return nil, fmt.Errorf("impersonation is not supported for the upstream config")
	}

	kubeClient, err := kubernetes.NewForConfig(upstreamRestConf)
	if err != nil {
		return nil, err
	}

	authenticationClient := kubeClient.AuthenticationV1()
	authorizationClient := kubeClient.AuthorizationV1()
	var lj injectIntoRequester
	switch ljConfig {
	case LabelInjectionNone:
		lj = noopLabelInjector{}
	case LabelInjectionClusterScoped, LabelInjectionAll:
		l, err := newLabelInjector(ctx, upstreamRestConf, serializer, labelInjectorMaxRequestBodySize, ljConfig == LabelInjectionClusterScoped)
		if err != nil {
			return nil, fmt.Errorf("failed to create label injector: %w", err)
		}
		lj = l
	}

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
		Rewrite: rewriteErrorWrapper(l, func(r *httputil.ProxyRequest) error {
			r.SetXForwarded()
			r.SetURL(upstreamURL)

			requestInfo, err := decodeRequestInfo(r.In)
			if err != nil {
				return fmt.Errorf("failed to decode request info: %w", err)
			}

			userInfo, err := extractUserInfoFromAuthHeader(r.In.Context(), authenticationClient, r.In.Header.Get("Authorization"))
			if err != nil {
				return fmt.Errorf("failed to extract user info from bearer token: %w", err)
			}

			scopeLabel, err := scopeExtractor.getScopeLabel(r.In.Header, userInfo.Username)
			if err != nil {
				return fmt.Errorf("failed to get scope label: %w", err)
			}

			if err := lj.InjectLabelIntoRequest(r.In.Context(), requestInfo, scopeLabel, r.Out); err != nil {
				return fmt.Errorf("failed to inject label: %w", err)
			}

			if !requestNeedsRewrite(requestInfo) {
				return nil
			}

			l.Info("Rewriting request", "scope", scopeLabel, "verb", requestInfo.Verb, "path", requestInfo.Path, "user-name", userInfo.Username, "user-uid", userInfo.UID)

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
			l.Error(err, "Proxy error", "path", req.URL.Path, "method", req.Method)

			statusCode := http.StatusInternalServerError
			if esc, ok := errors.AsType[*errorWithStatusCode](err); ok {
				statusCode = esc.statusCode
			}
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.WriteHeader(statusCode)
			_, _ = fmt.Fprintf(rw, "Proxy error: %s", err.Error())
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

			l.Info("Modifying SelfSubjectAccessReview response", "path", requestInfo.Path)

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
			l.V(5).Info("Decoded response body", "type", fmt.Sprintf("%T", decodedObj), "object", decodedObj)

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
						l.V(1).Info("SSAR response modified to allow access", "resource", obj.Spec.ResourceAttributes, "user-name", userInfo.Username, "user-uid", userInfo.UID, "scope", scopeLabel)
					}
					obj.Status.Allowed = allowed
					obj.Status.Denied = !allowed
					obj.Status.Reason = fmt.Sprintf("Access managed through chrysopoeia.io proxy for cluster-scoped resources: %s (Upstream reason: %s)", reason, obj.Status.Reason)
				}
				decodedObj = obj
			default:
				return fmt.Errorf("unexpected response type for SelfSubjectAccessReview: %T", obj)
			}

			if err := serializer.encodeIntoResponse(res, decodedObj); err != nil {
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

	if err := clientgoscheme.AddToScheme(scheme); err != nil {
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
		var code int
		if codeStr := r.Header.Get(internalErrorStatusCodeHeader); codeStr != "" {
			statusCode, err := strconv.Atoi(codeStr)
			if err == nil {
				code = statusCode
			}
		}
		if code == 0 {
			return nil, errors.New(errStr)
		} else {
			return nil, &errorWithStatusCode{
				error:      errors.New(errStr),
				statusCode: code,
			}
		}
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
				Verb:     ScopedListVerb,
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
	} else if after, ok := strings.CutPrefix(username, "system:serviceaccount:"); ok {
		// If the user is a service account, we can get the scope from an annotation on the service account. This is a fallback for when the request doesn't have the scope header.
		ns, name, ok := strings.Cut(after, ":")
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
func rewriteErrorWrapper(l logr.Logger, f func(*httputil.ProxyRequest) error) func(r *httputil.ProxyRequest) {
	return func(r *httputil.ProxyRequest) {
		r.Out.Header.Del(internalErrorHeader)
		r.Out.Header.Del(internalErrorStatusCodeHeader)
		if err := f(r); err != nil {
			l.Error(err, "Rewrite error", "path", r.In.URL.Path, "method", r.In.Method)
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

// creatorWithUnstructuredFallback allows falling back to [unstructured.Unstructured] if a kind is not registered with the scheme.
// We generally don't care about having a concrete type in the proxy (except for [authorizationv1.SelfSubjectAccessReview]) but
// protobuf serialization requires the concrete object so we can't use unstructured everywhere.
// Most clients use protobuf when requesting manifests from the groups built into the api server.
// The proxies client version should be equal or newer than the api-server version.
type creatorWithUnstructuredFallback struct {
	scheme *runtime.Scheme
}

// New tries [runtime.Scheme.New] and falls back to [unstructured.Unstructured] if the kind is not registered.
func (cuf creatorWithUnstructuredFallback) New(kind schema.GroupVersionKind) (out runtime.Object, err error) {
	obj, err := cuf.scheme.New(kind)
	if err != nil && runtime.IsNotRegisteredError(err) {
		u := new(unstructured.Unstructured)
		u.SetGroupVersionKind(kind)
		return u, nil
	} else if err != nil {
		return nil, err
	}
	return obj, nil
}

func newContentSerializer(scheme *runtime.Scheme) *contentSerializer {
	cuf := creatorWithUnstructuredFallback{scheme: scheme}
	return &contentSerializer{
		protoSerializer: protobuf.NewSerializer(cuf, scheme),
		jsonSerializer:  json.NewSerializerWithOptions(json.DefaultMetaFactory, cuf, nil, json.SerializerOptions{}),
		yamlSerializer:  json.NewSerializerWithOptions(json.DefaultMetaFactory, cuf, nil, json.SerializerOptions{Yaml: true}),
		cborSerializer:  cbor.NewSerializer(cuf, nil),
	}
}

type contentSerializer struct {
	protoSerializer *protobuf.Serializer
	jsonSerializer  *json.Serializer
	yamlSerializer  *json.Serializer
	cborSerializer  cbor.Serializer
}

func (e *contentSerializer) serializerForMediaType(mediaType string) (runtime.Serializer, error) {
	mediaType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return nil, fmt.Errorf("failed to parse media type: %w", err)
	}

	// https://kubernetes.io/docs/reference/using-api/api-concepts/#alternate-representations-of-resources
	var enc runtime.Serializer
	switch mediaType {
	case "application/json":
		enc = e.jsonSerializer
	case "application/yaml", "application/apply-patch+yaml":
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

func (e *contentSerializer) encodeIntoResponse(res *http.Response, obj runtime.Object) error {
	contentType := res.Header.Get("Content-Type")
	enc, err := e.serializerForMediaType(contentType)
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

func (e *contentSerializer) encodeIntoRequest(req *http.Request, obj runtime.Object) error {
	contentType := req.Header.Get("Content-Type")
	enc, err := e.serializerForMediaType(contentType)
	if err != nil {
		return fmt.Errorf("failed to get encoder for Content-Type %q: %w", contentType, err)
	}

	buf := &bytes.Buffer{}
	if err := enc.Encode(obj, buf); err != nil {
		return fmt.Errorf("failed to encode %s: %w", contentType, err)
	}

	req.Header.Del("Content-Length")
	req.Body = io.NopCloser(buf)
	req.ContentLength = int64(buf.Len())

	return nil
}

func groupVersionFromString(s string) schema.GroupVersion {
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		return schema.GroupVersion{Version: parts[0]}
	}
	return schema.GroupVersion{Group: parts[0], Version: parts[1]}
}

type injectIntoRequester interface {
	InjectLabelIntoRequest(context.Context, request.RequestInfo, string, *http.Request) error
}

type noopLabelInjector struct{}

func (noopLabelInjector) InjectLabelIntoRequest(context.Context, request.RequestInfo, string, *http.Request) error {
	return nil
}

type labelInjector struct {
	serializer         *contentSerializer
	mapper             *restmapper.DeferredDiscoveryRESTMapper
	ignoreGVR          sets.Set[schema.GroupVersionResource]
	clusterScopedOnly  bool
	maxRequestBodySize int64
	logger             logr.Logger
}

func newLabelInjector(ctx context.Context, upstreamRestConf *rest.Config, serializer *contentSerializer, maxRequestBodySize int64, clusterScopedOnly bool) (*labelInjector, error) {
	l := log.FromContext(ctx).WithName("labelInjector")
	discoClient, err := discovery.NewDiscoveryClientForConfig(upstreamRestConf)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapperWithContext(memory.NewMemCacheClientWithContext(discoClient))
	spr, err := discoClient.ServerPreferredResourcesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load server resource: %w", err)
	}
	gvrWithoutList := sets.New[schema.GroupVersionResource]()
	for _, rl := range spr {
		for _, r := range rl.APIResources {
			if slices.Contains(r.Verbs, "list") {
				continue
			}
			gvrWithoutList.Insert(groupVersionFromString(rl.GroupVersion).WithResource(r.Name))
		}
	}
	l.Info("Ignored GVRs when injecting labels", "stage", "init", "reason", "no list method", "gvr", gvrWithoutList.UnsortedList())

	return &labelInjector{
		serializer:         serializer,
		mapper:             mapper,
		ignoreGVR:          gvrWithoutList,
		clusterScopedOnly:  clusterScopedOnly,
		logger:             l,
		maxRequestBodySize: maxRequestBodySize,
	}, nil
}

// InjectLabelIntoRequest injects the given scope label into the request.
// It ignores any GVR without list verb. Those GVR are assumed to be "virtual" resources that do not support labels in their metadata.
// The most prominent ones are in the authorization.k8s.io group, for example [authorizationv1.SelfSubjectAccessReview].
func (lj *labelInjector) InjectLabelIntoRequest(ctx context.Context, requestInfo request.RequestInfo, scopeLabel string, req *http.Request) error {
	if !requestInfo.IsResourceRequest || requestInfo.Subresource != "" {
		return nil
	}
	if !slices.ContainsFunc([]string{"create", "update", "patch"}, func(v string) bool { return strings.EqualFold(v, requestInfo.Verb) }) {
		return nil
	}

	gvr := schema.GroupVersionResource{
		Group:    requestInfo.APIGroup,
		Version:  requestInfo.APIVersion,
		Resource: requestInfo.Resource,
	}
	if lj.ignoreGVR.Has(gvr) {
		lj.logger.Info("Ignore resource for body rewrite", "reason", "no list method", "scope", scopeLabel, "verb", requestInfo.Verb, "path", requestInfo.Path, "user-name")
		return nil
	}

	lj.logger.Info("Rewriting request body", "scope", scopeLabel, "verb", requestInfo.Verb, "path", requestInfo.Path, "user-name")

	rawBody, err := io.ReadAll(http.MaxBytesReader(nil, req.Body, lj.maxRequestBodySize))
	if err != nil {
		return fmt.Errorf("failed reading body for rewriting: %w", err)
	}
	gvk, err := lj.mapper.KindForWithContext(ctx, gvr)
	if err != nil {
		return fmt.Errorf("failed to translate resource to GVK: %w", err)
	}
	s, err := lj.serializer.serializerForMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("failed to get serializer for content type: %w", err)
	}
	o, _, err := s.Decode(rawBody, &gvk, nil)
	if err != nil {
		return fmt.Errorf("failed to decode request body: %w", err)
	}
	co, ok := o.(client.Object)
	if !ok {
		return fmt.Errorf("failed to cast object %T to client.Object", co)
	}
	labels := co.GetLabels()
	if _, ok := labels[scopeLabel]; !ok {
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[scopeLabel] = ""
	}
	co.SetLabels(labels)
	if err := lj.serializer.encodeIntoRequest(req, co); err != nil {
		return fmt.Errorf("failed to encode rewritten body: %w", err)
	}

	return nil
}
