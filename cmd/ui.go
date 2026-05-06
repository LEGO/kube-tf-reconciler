package cmd

import (
	"fmt"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/ui"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var uiNamespace string
var uiPort int

//nolint:exhaustruct,gochecknoglobals
var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Serve a web GUI for watching Workspace resources",
	RunE:  runUI,
}

func init() {
	rootCmd.AddCommand(uiCmd)
	uiCmd.Flags().StringVarP(&uiNamespace, "namespace", "n", "", "Namespace to watch (empty = all namespaces)")
	uiCmd.Flags().IntVarP(&uiPort, "port", "p", 7777, "Port to listen on")
}

func runUI(cmd *cobra.Command, _ []string) error {
	uiScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(uiScheme))
	utilruntime.Must(tfreconcilev1alpha1.AddToScheme(uiScheme))

	cfg := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(cfg, client.Options{Scheme: uiScheme})
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	return ui.Run(cmd.Context(), k8sClient, uiNamespace, uiPort)
}
