package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
)

type Exec struct {
	RootDir       string
	installDir    string
	WorkspacesDir string
}

func New(rootDir string) *Exec {
	var err error
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		panic(fmt.Errorf("failed to get absolute path for root dir: %w", err))
	}

	err = os.MkdirAll(rootDir, 0755)
	if err != nil {
		panic(err)
	}

	installDir := filepath.Join(rootDir, "installs")
	workspacesDir := filepath.Join(rootDir, "workspaces")
	err = os.MkdirAll(installDir, 0755)
	if err != nil {
		panic(err)
	}
	err = os.MkdirAll(workspacesDir, 0755)
	if err != nil {
		panic(err)
	}

	return &Exec{
		RootDir:       rootDir,
		installDir:    installDir,
		WorkspacesDir: workspacesDir,
	}
}

func (e *Exec) SetupWorkspace(ws string) (string, error) {
	fullPath := filepath.Join(e.WorkspacesDir, ws)
	err := os.MkdirAll(fullPath, 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create workspace dir: %w", err)
	}

	return fullPath, nil
}

// SetupTerraformRC creates a .terraformrc file in the workspace directory if content is provided
func (e *Exec) SetupTerraformRC(workspacePath string, terraformRCContent string) (string, error) {
	if terraformRCContent == "" {
		return "", nil // No custom .terraformrc provided
	}

	// Create the config file in the workspace's directory to isolate configuration
	terraformRCPath := filepath.Join(workspacePath, ".terraformrc")

	err := os.WriteFile(terraformRCPath, []byte(terraformRCContent), 0600)
	if err != nil {
		return "", fmt.Errorf("failed to write .terraformrc file: %w", err)
	}

	return terraformRCPath, nil
}

func (e *Exec) GetTerraformForWorkspace(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (*tfexec.Terraform, string, error) {
	path, err := e.SetupWorkspace(filepath.Join(ws.Namespace, ws.Name))
	if err != nil {
		return nil, "", fmt.Errorf("failed to setup workspace: %w", err)
	}

	var terraformRCPath string
	if ws.Spec.TerraformRC != "" {
		terraformRCPath, err = e.SetupTerraformRC(path, ws.Spec.TerraformRC)
		if err != nil {
			return nil, "", fmt.Errorf("failed to setup .terraformrc: %w", err)
		}
	}

	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		InstallDir: e.installDir,
		Version:    version.Must(version.NewVersion(ws.Spec.TerraformVersion)),
	}

	//custom timeout because Openshift is slow
	installer.Timeout = 2 * time.Minute

	execPath, err := installer.Install(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("failed to install terraform: %w", err)
	}
	tf, err := tfexec.NewTerraform(path, execPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create terraform instance: %w", err)
	}

	return tf, terraformRCPath, nil
}
