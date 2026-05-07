package cmd

import (
	"fmt"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/ui"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var uiNamespace string
var uiPort int
var uiContext string

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
	uiCmd.Flags().StringVarP(&uiContext, "context", "c", "", "Kubeconfig context to use (default: current context)")
}

func runUI(cmd *cobra.Command, _ []string) error {
	uiScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(uiScheme))
	utilruntime.Must(tfreconcilev1alpha1.AddToScheme(uiScheme))

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if uiContext != "" {
		overrides.CurrentContext = uiContext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	k8sClient, err := client.New(cfg, client.Options{Scheme: uiScheme})
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	return ui.Run(cmd.Context(), k8sClient, uiNamespace, uiContext, uiPort)
}
