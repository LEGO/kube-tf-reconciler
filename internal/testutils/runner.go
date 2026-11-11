package testutils

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimachinerywait "k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/e2e-framework/klient/wait"
)

// ToolCacheDirEnv names the folder for Terraform installs and providers that tests share.
// If it is not set, the tests use krec-test in the user cache folder.
const ToolCacheDirEnv = "KREC_TEST_CACHE_DIR"

// The runner keeps these folders below its root (see runner.New).
var sharedRunnerDirs = []string{"installs", "plugin-cache"}

var toolCacheDir = sync.OnceValues(func() (string, error) {
	dir := os.Getenv(ToolCacheDirEnv)
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "krec-test")
	}
	return filepath.Abs(dir)
})

// RunnerRoot returns a new root for runner.New and render.NewFileRender. Workspaces stay
// private to t, but installs and the plugin cache link to the shared tool cache.
//
// The Terraform plugin cache is not safe for concurrent writes. Do not use RunnerRoot in
// top-level tests that run in parallel.
func RunnerRoot(t *testing.T) string {
	t.Helper()
	cache, err := toolCacheDir()
	require.NoError(t, err)

	root := t.TempDir()
	for _, name := range sharedRunnerDirs {
		shared := filepath.Join(cache, name)
		require.NoError(t, os.MkdirAll(shared, 0755))
		require.NoError(t, os.Symlink(shared, filepath.Join(root, name)))
	}
	return root
}

// WaitFor is wait.For with a short poll interval and a check before the first wait.
// The defaults of wait.For wait 5s before each check, also when the condition is already true.
func WaitFor(condition apimachinerywait.ConditionWithContextFunc, opts ...wait.Option) error {
	defaults := []wait.Option{wait.WithInterval(250 * time.Millisecond), wait.WithImmediate()}
	return wait.For(condition, append(defaults, opts...)...)
}
