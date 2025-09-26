package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/terraform-exec/tfexec"
)

type Exec struct {
	RootDir        string
	installDir     string
	WorkspacesDir  string
	PluginCacheDir string

	terraformInstalledVersions map[string]string
	terraformInstallMutex      sync.RWMutex
	providerInitMutex          sync.Mutex
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
	pluginCacheDir := filepath.Join(rootDir, "plugin-cache")
	err = os.MkdirAll(pluginCacheDir, 0755)
	if err != nil {
		panic(err)
	}

	return &Exec{
		RootDir:                    rootDir,
		installDir:                 installDir,
		WorkspacesDir:              workspacesDir,
		PluginCacheDir:             pluginCacheDir,
		terraformInstalledVersions: make(map[string]string),
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

func (e *Exec) SetupTerraformRC(workspacePath string, terraformRCContent string) (string, error) {
	if terraformRCContent == "" {
		return "", nil
	}

	// Create the config file in the workspace's directory to isolate configuration
	terraformRCPath := filepath.Join(workspacePath, ".terraformrc")

	err := os.WriteFile(terraformRCPath, []byte(terraformRCContent), 0600)
	if err != nil {
		return "", fmt.Errorf("failed to write .terraformrc file: %w", err)
	}

	return terraformRCPath, nil
}

func (e *Exec) TerraformInit(ctx context.Context, tf *tfexec.Terraform, opts ...tfexec.InitOption) error {
	e.providerInitMutex.Lock()
	defer e.providerInitMutex.Unlock()
	err := tf.Init(ctx, opts...)
	return err
}

func (e *Exec) getTerraformBinary(ctx context.Context, terraformVersion string) (string, error) {
	e.terraformInstallMutex.Lock()
	defer e.terraformInstallMutex.Unlock()
	if execPath, exists := e.terraformInstalledVersions[terraformVersion]; exists {
		// Verify the binary still exists
		if _, err := os.Stat(execPath); err == nil {
			return execPath, nil
		}
		// If it doesn't exist or was deleted, remove from cache
		delete(e.terraformInstalledVersions, terraformVersion)
	}

	// Not installed or missing — do the install
	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		InstallDir: e.installDir,
		Version:    version.Must(version.NewVersion(terraformVersion)),
	}
	installer.Timeout = 2 * time.Minute

	execPath, err := installer.Install(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to install terraform: %w", err)
	}

	e.terraformInstalledVersions[terraformVersion] = execPath
	return execPath, nil
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

	execPath, err := e.getTerraformBinary(ctx, ws.Spec.TerraformVersion)
	if err != nil {
		return nil, "", err
	}

	tf, err := tfexec.NewTerraform(path, execPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create terraform instance: %w", err)
	}

	return tf, terraformRCPath, nil
}

func (e *Exec) CleanupWorkspace(ws tfreconcilev1alpha1.Workspace) error {
	workspacePath := filepath.Join(e.WorkspacesDir, ws.Namespace, ws.Name)

	if _, err := os.Stat(workspacePath); os.IsNotExist(err) {
		return nil
	}

	err := os.RemoveAll(workspacePath)
	if err != nil {
		return fmt.Errorf("failed to remove workspace directory %s: %w", workspacePath, err)
	}

	return nil
}
