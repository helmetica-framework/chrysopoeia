package cmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"
	"go.uber.org/multierr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/helmetica-framework/chrysopoeia/proxy"
)

func init() {
	RootCmd.AddCommand(proxyCmd)

	proxyCmd.Flags().Bool("secure", true, "Whether to use HTTPS for the proxy server.")
	proxyCmd.Flags().String("addr", ":8080", "The address that the proxy will listen on.")

	proxyCmd.Flags().String("inject-label", "all", "Controls whether the proxy injects the scope label on create and update requests. One of: [all, cluster-scoped, none]")
	proxyCmd.Flags().Int64("inject-label-max-request-body-size-bytes", 5*1024*1024, "The max body size when rewriting the request body to include the label")

	proxyCmd.Flags().String("cert-path", "", "The directory that contains the webhook certificate.")
	proxyCmd.Flags().String("cert-name", "tls.crt", "The name of the webhook certificate file.")
	proxyCmd.Flags().String("cert-key", "tls.key", "The name of the webhook key file.")
}

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Starts the proxy",
	Long:  "Starts the proxy",
	RunE:  runProxy,
}

func runProxy(cmd *cobra.Command, _ []string) error {
	secure, secErr := cmd.Flags().GetBool("secure")
	addr, addrErr := cmd.Flags().GetString("addr")
	proxyCertPath, pcperr := cmd.Flags().GetString("cert-path")
	proxyCertName, pcnerr := cmd.Flags().GetString("cert-name")
	proxyCertKey, pckerr := cmd.Flags().GetString("cert-key")
	maxRequestBody, mrberr := cmd.Flags().GetInt64("inject-label-max-request-body-size-bytes")

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if err := multierr.Combine(secErr, addrErr, pcperr, pcnerr, pckerr, mrberr); err != nil {
		return fmt.Errorf("failed to get flags: %w", err)
	}
	var liConf proxy.LabelInjection
	switch f, _ := cmd.Flags().GetString("inject-label"); f {
	case "all":
		liConf = proxy.LabelInjectionAll
	case "cluster-scoped":
		liConf = proxy.LabelInjectionClusterScoped
	case "none":
		liConf = proxy.LabelInjectionNone
	default:
		return fmt.Errorf("unknown --inject-label configuration %q", f)
	}

	var webhookCertWatcher *certwatcher.CertWatcher

	var webhookTLSOpts *tls.Config
	if len(proxyCertPath) > 0 {
		cmd.Println("Initializing certificate watcher using provided certificates",
			"cert-path", proxyCertPath, "cert-name", proxyCertName, "cert-key", proxyCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(proxyCertPath, proxyCertName),
			filepath.Join(proxyCertPath, proxyCertKey),
		)
		if err != nil {
			return fmt.Errorf("failed to initialize webhook certificate watcher: %w", err)
		}

		webhookTLSOpts = &tls.Config{
			GetCertificate: webhookCertWatcher.GetCertificate,
		}
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	p, err := proxy.New(ctx, ctrl.GetConfigOrDie(), liConf, maxRequestBody)
	if err != nil {
		return fmt.Errorf("failed to create proxy: %w", err)
	}

	s := &http.Server{
		TLSConfig: webhookTLSOpts,
		Addr:      addr,
		Handler:   p,
	}

	if secure {
		return s.ListenAndServeTLS("", "")
	}
	return s.ListenAndServe()
}
