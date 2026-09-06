package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/ui"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var uiNamespace string
var uiPort int
var uiContext string
var uiInCluster bool

var (
	uiOAuthIssuer       string
	uiOAuthClientID     string
	uiOAuthClientSecret string
	uiOAuthRedirectURL  string
	uiOAuthScopes       string
	uiSessionSecret     string
)

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
	uiCmd.Flags().BoolVar(&uiInCluster, "in-cluster", false, "Run in-cluster using the pod service account instead of kubeconfig")
	uiCmd.Flags().StringVar(&uiOAuthIssuer, "oauth-issuer", "", "OIDC issuer URL (enables authentication)")
	uiCmd.Flags().StringVar(&uiOAuthClientID, "oauth-client-id", "", "OAuth2 client ID")
	uiCmd.Flags().StringVar(&uiOAuthClientSecret, "oauth-client-secret", "", "OAuth2 client secret")
	uiCmd.Flags().StringVar(&uiOAuthRedirectURL, "oauth-redirect-url", "", "OAuth2 redirect URL (default: derived from request)")
	uiCmd.Flags().StringVar(&uiOAuthScopes, "oauth-scopes", "openid profile email", "Comma-separated OAuth2 scopes")
	uiCmd.Flags().StringVar(&uiSessionSecret, "session-secret", "", "Secret used to sign session cookies (default: ephemeral)")
}

func runUI(cmd *cobra.Command, _ []string) error {
	uiScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(uiScheme))
	utilruntime.Must(tfreconcilev1alpha1.AddToScheme(uiScheme))

	authCfg, err := buildAuthConfig(cmd)
	if err != nil {
		return err
	}

	var cfg *rest.Config
	if uiInCluster {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to load in-cluster config: %w", err)
		}
	} else {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		overrides := &clientcmd.ConfigOverrides{}
		if uiContext != "" {
			overrides.CurrentContext = uiContext
		}
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
		if err != nil {
			return fmt.Errorf("failed to load kubeconfig: %w", err)
		}
	}
	k8sClient, err := client.New(cfg, client.Options{Scheme: uiScheme})
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	return ui.Run(cmd.Context(), k8sClient, uiNamespace, uiContext, uiPort, uiInCluster, authCfg)
}

func envOr(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}

func buildAuthConfig(cmd *cobra.Command) (*ui.AuthConfig, error) {
	issuer := envOr(uiOAuthIssuer, "KREC_UI_OAUTH_ISSUER")
	clientID := envOr(uiOAuthClientID, "KREC_UI_OAUTH_CLIENT_ID")
	clientSecret := envOr(uiOAuthClientSecret, "KREC_UI_OAUTH_CLIENT_SECRET")
	if issuer == "" && clientID == "" && clientSecret == "" {
		return nil, nil
	}
	if issuer == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("oauth authentication requires oauth-issuer, oauth-client-id and oauth-client-secret")
	}

	scopes := envOr(uiOAuthScopes, "KREC_UI_OAUTH_SCOPES")
	if !cmd.Flags().Changed("oauth-scopes") {
		if v := os.Getenv("KREC_UI_OAUTH_SCOPES"); v != "" {
			scopes = v
		}
	}
	var scopeList []string
	for _, s := range strings.Split(scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopeList = append(scopeList, s)
		}
	}

	cfg := &ui.AuthConfig{
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  envOr(uiOAuthRedirectURL, "KREC_UI_OAUTH_REDIRECT_URL"),
		Scopes:       scopeList,
	}
	if sessionSecret := envOr(uiSessionSecret, "KREC_UI_SESSION_SECRET"); sessionSecret != "" {
		cfg.SessionSecret = []byte(sessionSecret)
	}
	return cfg, nil
}
