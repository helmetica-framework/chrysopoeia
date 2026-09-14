package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// fetchChart downloads and loads the Helm chart behind a Flux artifact URL.
//
// If hostnameOverride is set it replaces the host of the artifact URL, keeping scheme and path.
// That is how a controller running outside the cluster reaches the in-cluster source controller.
//
// The returned chart carries its Raw files, which is where the unparsed values.yaml lives: both
// the schema generator and the values pre-processor read the comments and cel: expressions that
// the parsed chart.Values has already thrown away.
//
// The request is bound to ctx, so a source controller that accepts the connection and then goes
// quiet cannot hold a reconcile worker past the manager's shutdown. It has no deadline of its own:
// http.DefaultClient sets no timeout, and neither does the default transport for reading a
// response body.
func fetchChart(ctx context.Context, artifactURL, hostnameOverride string) (*chartv2.Chart, error) {
	if artifactURL == "" {
		return nil, fmt.Errorf("artifactURL is required")
	}

	if hostnameOverride != "" {
		aurl, err := url.Parse(artifactURL)
		if err != nil {
			return nil, fmt.Errorf("artifactURL %s: %w", artifactURL, err)
		}
		aurl.Host = hostnameOverride
		artifactURL = aurl.String()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating the http request: %w", err)
	}

	chartClient := &http.Client{Timeout: 2 * time.Minute}

	res, err := chartClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error fetching chart: %s", res.Status)
	}

	return loader.LoadArchive(res.Body)
}
