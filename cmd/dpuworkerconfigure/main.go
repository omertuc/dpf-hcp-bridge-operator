package dpuworkerconfigure

import (
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dpu-worker-configure",
		Short: "Configure networking on DPU worker nodes",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			ctrl.SetLogger(zap.New())
		},
	}

	cmd.AddCommand(newSetupCommand())
	cmd.AddCommand(newP0RoutingCommand())

	return cmd
}

func newSetupCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "One-shot bridge creation, NM unmanage, and OVS disable",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup()
		},
	}
}

func newP0RoutingCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "p0-routing",
		Short: "Long-running routing table 100 reconciliation",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runP0Routing()
		},
	}
}
