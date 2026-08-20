package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
)

// serveChart packages testdata/celwrapper the way Flux serves an artifact and returns its URL.
func serveChart(t *testing.T) string {
	t.Helper()

	chartLoader, err := loader.Loader("./testdata/celwrapper")
	require.NoError(t, err)
	chart, err := chartLoader.Load()
	require.NoError(t, err)

	archive, err := chartutil.Save(chart, t.TempDir())
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chart.tgz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, archive)
	}))
	t.Cleanup(srv.Close)

	return srv.URL + "/chart.tgz"
}

func TestFetchChart(t *testing.T) {
	chart, err := fetchChart(t.Context(), serveChart(t), "")
	require.NoError(t, err)

	assert.Equal(t, "celwrapper", chart.Name())
	assert.NotEmpty(t, chart.Raw, "the raw values.yaml has to survive, the pre-processor reads it")

	var values []byte
	for _, f := range chart.Raw {
		if f.Name == "values.yaml" {
			values = f.Data
		}
	}
	require.NotNil(t, values, "values.yaml is in the archive")
	assert.Contains(t, string(values), "cel: values.ingressHostname",
		"the expression reaches the caller verbatim, comments and all")
}

func TestFetchChart_HostnameOverride(t *testing.T) {
	// In-cluster the artifact URL names the source controller service, which a test or a local run
	// cannot resolve. The override replaces the host and keeps the path.
	parsed, err := url.Parse(serveChart(t))
	require.NoError(t, err)

	chart, err := fetchChart(t.Context(), "http://source-controller.flux-system.svc/chart.tgz", parsed.Host)
	require.NoError(t, err)
	assert.Equal(t, "celwrapper", chart.Name())
}

func TestFetchChart_NotFound(t *testing.T) {
	_, err := fetchChart(t.Context(), serveChart(t)+"/missing", "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "404")
}

func TestFetchChart_EmptyURL(t *testing.T) {
	_, err := fetchChart(t.Context(), "", "")
	require.Error(t, err)
}

func TestFetchChart_NotAnArchive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not a gzipped tarball"))
	}))
	t.Cleanup(srv.Close)

	_, err := fetchChart(t.Context(), srv.URL+"/chart.tgz", "")
	require.Error(t, err)
}

func TestFetchChart_HonoursContextCancellation(t *testing.T) {
	// The fetch runs inside a reconcile: when the manager shuts down, it has to stop rather than
	// block the worker until the server answers.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fetchChart(ctx, srv.URL+"/chart.tgz", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
