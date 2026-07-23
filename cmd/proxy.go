package cmd

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/helmetica-framework/chrysopoeia/proxy"
	"github.com/spf13/cobra"
	"go.uber.org/multierr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	//+kubebuilder:scaffold:imports
)

func init() {
	RootCmd.AddCommand(proxyCmd)

	proxyCmd.Flags().Bool("secure", true, "Whether to use HTTPS for the proxy server.")
	proxyCmd.Flags().String("addr", ":8080", "The address that the proxy will listen on.")

	proxyCmd.Flags().String("cert-path", "", "The directory that contains the webhook certificate.")
	proxyCmd.Flags().String("cert-name", "tls.crt", "The name of the webhook certificate file.")
	proxyCmd.Flags().String("cert-key", "tls.key", "The name of the webhook key file.")

	proxyCmd.Flags().String("injected-label", "reserved.chrysopoeia.io/non-matching", "The label that will be injected into the cluster scoped queries.")
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
	injectedLabel, ilerr := cmd.Flags().GetString("injected-label")

	if err := multierr.Combine(secErr, addrErr, pcperr, pcnerr, pckerr, ilerr); err != nil {
		return fmt.Errorf("failed to get flags: %w", err)
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

	p, err := proxy.New(ctrl.GetConfigOrDie(), injectedLabel)
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
