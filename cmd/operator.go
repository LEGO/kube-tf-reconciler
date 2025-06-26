package cmd

import (
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
	"lukaspj.io/kube-tf-reconciler/internal/controller"
	"lukaspj.io/kube-tf-reconciler/pkg/runner"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tfreconcilev1alpha1.AddToScheme(scheme))
}

// operatorCmd represents the operator command
//
//nolint:exhaustruct
var operatorCmd = &cobra.Command{
	Use:   "operator",
	Short: "Operator for managing terraform resources in Kubernetes.",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := ConfigFromEnvironment()
		if err != nil {
			slog.Error("unable to load config", "error", err)
			os.Exit(1)
		}

		slog.Info("config loaded", "config", cfg)

		slog.SetLogLoggerLevel(slog.LevelInfo)
		ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

		mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme:                  scheme,
			HealthProbeBindAddress:  cfg.ProbeAddr,
			LeaderElectionNamespace: cfg.Namespace,
			LeaderElection:          cfg.EnableLeaderElection,
			LeaderElectionID:        "69943c0d.krec-operator.lukasjp",
		})
		if err != nil {
			slog.Error("unable to start manager", "error", err)
			os.Exit(1)
		}

		reconciler := &controller.WorkspaceReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorderFor("krec"),

			Tf: runner.New(cfg.WorkspacePath),
		}

		if err = reconciler.SetupWithManager(mgr); err != nil {
			slog.Error("unable to create controller", "error", err)
			os.Exit(1)
		}

		if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			slog.Error("unable to set up health check", "error", err)
			os.Exit(1)
		}
		if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			slog.Error("unable to set up ready check", "error", err)
			os.Exit(1)
		}

		slog.Info("starting manager")
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			slog.Error("problem running manager", "error", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(operatorCmd)
}
