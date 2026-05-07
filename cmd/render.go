//nolint:gochecknoinits
package cmd

import (
	"fmt"
	"os"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/spf13/cobra"
	sigsyaml "sigs.k8s.io/yaml"
)

//nolint:gochecknoglobals
var (
	renderFile   string
	renderOutput string
)

//nolint:exhaustruct,gochecknoglobals
var renderCmd = &cobra.Command{
	Use:   "render",
	Short: "Render a Workspace manifest to HCL",
	RunE:  runRender,
}

func runRender(cmd *cobra.Command, _ []string) error {
	raw, err := os.ReadFile(renderFile)
	if err != nil {
		return fmt.Errorf("reading file %s: %w", renderFile, err)
	}

	var ws tfreconcilev1alpha1.Workspace
	if err := sigsyaml.Unmarshal(raw, &ws); err != nil {
		return fmt.Errorf("unmarshalling workspace: %w", err)
	}

	f := hclwrite.NewEmptyFile()

	if err := render.Workspace(f.Body(), ws); err != nil {
		return fmt.Errorf("rendering workspace: %w", err)
	}

	if err := render.Providers(f.Body(), ws.Spec.ProviderSpecs); err != nil {
		return fmt.Errorf("rendering providers: %w", err)
	}

	if ws.Spec.Module != nil {
		if err := render.Module(f.Body(), ws.Spec.Module); err != nil {
			return fmt.Errorf("rendering module: %w", err)
		}
	}

	if renderOutput == "" {
		fmt.Fprint(cmd.OutOrStdout(), string(f.Bytes()))
		return nil
	}

	if err := os.WriteFile(renderOutput, f.Bytes(), 0644); err != nil { //nolint:gosec
		return fmt.Errorf("writing output file %s: %w", renderOutput, err)
	}

	return nil
}

func init() {
	renderCmd.Flags().StringVarP(&renderFile, "file", "f", "", "path to Workspace YAML manifest")
	renderCmd.Flags().StringVarP(&renderOutput, "output", "o", "", "write HCL to this file (default: stdout)")

	_ = renderCmd.MarkFlagRequired("file")

	rootCmd.AddCommand(renderCmd)
}
