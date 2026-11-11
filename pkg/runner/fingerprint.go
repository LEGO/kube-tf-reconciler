package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/hashicorp/go-version"
)

const (
	initMarkerFile     = ".krec-init.json"
	fingerprintVersion = "v1"
)

var (
	gitSHARef           = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	registryModuleRegex = regexp.MustCompile(`^([A-Za-z0-9.-]+\.[A-Za-z]{2,}(:[0-9]+)?/)?[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+$`)
)

type InitMarker struct {
	Fingerprint   string    `json:"fingerprint"`
	Generation    int64     `json:"generation"`
	BootID        string    `json:"bootID"`
	InitializedAt time.Time `json:"initializedAt"`
}

func IsPinnedModuleSource(source, constraint string) bool {
	source = strings.TrimSpace(source)
	if source == "" {
		return false
	}

	if isGitSource(source) {
		return gitSHARef.MatchString(gitRef(source))
	}

	if isRegistrySource(source) {
		return isExactVersion(constraint)
	}

	return false
}

func isRelativeSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../")
}

func isGitSource(source string) bool {
	for _, prefix := range []string{"git::", "github.com/", "bitbucket.org/", "git@"} {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return false
}

func gitRef(source string) string {
	idx := strings.LastIndex(source, "?")
	if idx < 0 {
		return ""
	}
	q, err := url.ParseQuery(source[idx+1:])
	if err != nil {
		return ""
	}
	return q.Get("ref")
}

func isRegistrySource(source string) bool {
	if strings.Contains(source, "::") || strings.Contains(source, "?") || isRelativeSource(source) || strings.HasPrefix(source, "/") {
		return false
	}
	if idx := strings.Index(source, "//"); idx >= 0 {
		source = source[:idx]
	}
	return registryModuleRegex.MatchString(source)
}

func isExactVersion(constraint string) bool {
	c := strings.TrimSpace(constraint)
	if c == "" || strings.ContainsAny(c, ",<>~!") {
		return false
	}
	c = strings.TrimSpace(strings.TrimPrefix(c, "="))
	_, err := version.NewVersion(c)
	return err == nil
}

type modulesManifest struct {
	Modules []struct {
		Key     string `json:"Key"`
		Source  string `json:"Source"`
		Version string `json:"Version"`
		Dir     string `json:"Dir"`
	} `json:"Modules"`
}

var ErrModulesNotInstalled = errors.New("modules not installed")

// NestedModulesPinned checks modules below the root module call. Nested registry modules count as floating because
// modules.json doesn't record their version constraint.
func (e *Exec) NestedModulesPinned(ws *tfreconcilev1alpha1.Workspace) (bool, error) {
	wsPath := e.GetWorkspacePath(ws)
	raw, err := os.ReadFile(filepath.Join(wsPath, ".terraform", "modules", "modules.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, ErrModulesNotInstalled
		}
		return false, err
	}

	var manifest modulesManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, fmt.Errorf("failed to parse modules.json: %w", err)
	}

	pinned := true
	for _, m := range manifest.Modules {
		if m.Key == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(wsPath, m.Dir)); err != nil {
			return false, fmt.Errorf("%w: %s", ErrModulesNotInstalled, m.Dir)
		}
		// The root module call is checked from the spec, relative children are covered by their parent's pin
		if !strings.Contains(m.Key, ".") || isRelativeSource(m.Source) {
			continue
		}
		if !IsPinnedModuleSource(m.Source, "") {
			pinned = false
		}
	}

	return pinned, nil
}

func (e *Exec) InitFingerprint(ws *tfreconcilev1alpha1.Workspace, env map[string]string) (string, error) {
	wsPath := e.GetWorkspacePath(ws)
	tool, toolVersion := ResolveTool(ws)

	h := sha256.New()
	write := func(name string, value []byte) {
		_, _ = fmt.Fprintf(h, "%s:%d:", name, len(value))
		_, _ = h.Write(value)
		_, _ = h.Write([]byte{0})
	}

	write("fingerprint-version", []byte(fingerprintVersion))
	write("tool", []byte(tool))
	write("tool-version", []byte(toolVersion))
	write("terraformrc", []byte(ws.Spec.TerraformRC))

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Keep fingerprints comparable between replicas
		v := strings.ReplaceAll(env[k], e.RootDir, "$KREC_ROOT")
		write("env", []byte(k+"="+v))
	}

	for _, f := range []struct {
		name     string
		path     string
		required bool
	}{
		{"main.tf", filepath.Join(wsPath, "main.tf"), true},
		{"lock", filepath.Join(wsPath, ".terraform.lock.hcl"), false},
		{"modules", filepath.Join(wsPath, ".terraform", "modules", "modules.json"), true},
	} {
		content, err := os.ReadFile(f.path)
		if err != nil {
			if os.IsNotExist(err) && !f.required {
				content = nil
			} else if os.IsNotExist(err) {
				return "", fmt.Errorf("%w: %s", ErrModulesNotInstalled, f.name)
			} else {
				return "", fmt.Errorf("failed to read %s: %w", f.name, err)
			}
		}
		write(f.name, content)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *Exec) ReadInitMarker(ws *tfreconcilev1alpha1.Workspace) (*InitMarker, error) {
	raw, err := os.ReadFile(filepath.Join(e.GetWorkspacePath(ws), initMarkerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m InitMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil
	}
	return &m, nil
}

func (e *Exec) WriteInitMarker(ws *tfreconcilev1alpha1.Workspace, fingerprint string) error {
	raw, err := json.Marshal(InitMarker{
		Fingerprint:   fingerprint,
		Generation:    ws.Generation,
		BootID:        e.BootID,
		InitializedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.GetWorkspacePath(ws), initMarkerFile), raw, 0600)
}

func (e *Exec) RemoveInitMarker(ws *tfreconcilev1alpha1.Workspace) error {
	err := os.Remove(filepath.Join(e.GetWorkspacePath(ws), initMarkerFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func ResolveTool(ws *tfreconcilev1alpha1.Workspace) (tfreconcilev1alpha1.Tool, string) {
	tool := ws.Spec.Tool
	if tool == "" {
		tool = tfreconcilev1alpha1.ToolTerraform
	}

	iacVersion := ws.Spec.ToolVersion
	if iacVersion == "" && tool == tfreconcilev1alpha1.ToolTerraform {
		// backwards compatibility for the deprecated terraformVersion field
		iacVersion = ws.Spec.TerraformVersion
	}
	return tool, iacVersion
}
