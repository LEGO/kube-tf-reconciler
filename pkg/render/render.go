package render

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

type Renderer interface {
	Render(ws *tfreconcilev1alpha1.Workspace) (string, error)
}

type FileRender struct {
	RootDir string
}

var _ Renderer = (*FileRender)(nil)

func NewFileRender(rootDir string) *FileRender {
	err := os.MkdirAll(rootDir, 0755)
	if err != nil {
		panic(fmt.Errorf("failed to create workspace dir: %w", err))
	}
	return &FileRender{
		RootDir: rootDir,
	}
}

func (fr *FileRender) Render(ws *tfreconcilev1alpha1.Workspace) (string, error) {
	err := os.MkdirAll(filepath.Join(fr.RootDir, "workspaces", ws.Namespace, ws.Name), 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create workspace dir: %w", err)
	}

	f := hclwrite.NewEmptyFile()
	err = Workspace(f.Body(), *ws)
	renderErr := fmt.Errorf("failed to render workspace %s/%s", ws.Namespace, ws.Name)
	if err != nil {
		return "", fmt.Errorf("%w: %w", renderErr, err)
	}

	err = Providers(f.Body(), ws.Spec.ProviderSpecs)
	if err != nil {
		return "", fmt.Errorf("%w: failed to render providers: %w", renderErr, err)
	}

	err = Module(f.Body(), ws.Spec.Module)
	if err != nil {
		return "", fmt.Errorf("%w: failed to render module: %w", renderErr, err)
	}

	err = os.WriteFile(filepath.Join(fr.RootDir, "workspaces", ws.Namespace, ws.Name, "main.tf"), f.Bytes(), 0644)
	if err != nil {
		return "", fmt.Errorf("%w: failed to write workspace: %w", renderErr, err)
	}

	return string(f.Bytes()), nil
}

type LocalModuleRenderer struct {
	FileRender
	modules []string
	tmpPath string
}

func NewLocalModuleRenderer(rootDir string, modules map[string][]byte) *LocalModuleRenderer {
	tmpDir := filepath.Join(rootDir, "__localModules")
	for path, content := range modules {
		fullPath := filepath.Join(tmpDir, path, "main.tf")
		dir := filepath.Dir(fullPath)

		err := os.MkdirAll(dir, 0755)
		if err != nil {
			panic(fmt.Errorf("failed to create module dir: %w", err))
		}

		err = os.WriteFile(fullPath, content, 0644)
		if err != nil {
			panic(fmt.Errorf("failed to write module file: %w", err))
		}
	}

	return &LocalModuleRenderer{
		FileRender: *NewFileRender(rootDir),
		modules:    slices.Collect(maps.Keys(modules)),
		tmpPath:    tmpDir,
	}
}

func (lmr *LocalModuleRenderer) Render(ws *tfreconcilev1alpha1.Workspace) (string, error) {
	if ws.Spec.Module != nil && len(lmr.modules) > 0 && slices.Contains(lmr.modules, ws.Spec.Module.Source) {
		ws.Spec.Module.Source = filepath.Join(lmr.tmpPath, ws.Spec.Module.Source)
	}

	return lmr.FileRender.Render(ws)
}
