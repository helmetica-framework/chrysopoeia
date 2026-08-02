package cmd

import (
	"flag"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	zapOpts = zap.Options{
		Development: true,
	}
	zapFlagSet = flag.NewFlagSet("zap", flag.ExitOnError)
)

func init() {
	zapOpts.BindFlags(zapFlagSet)
	RootCmd.PersistentFlags().AddGoFlagSet(zapFlagSet)
}

var RootCmd = &cobra.Command{
	Use:   "chrysopoeia",
	Short: "chrysopoeia creates CRDs from Helm charts.",
	Long:  "chrysopoeia creates CRDs from Helm charts.",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		cmd.SilenceUsage = true
	},
}

func Execute() {
	lifetimeCtx := ctrl.SetupSignalHandler()

	RootCmd.ExecuteContext(lifetimeCtx)
}
