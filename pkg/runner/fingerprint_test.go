package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
)

const sha = "605beb1ed65a00b79edbcb74ff851d92399f598d"

func TestIsPinnedModuleSource(t *testing.T) {
	tests := []struct {
		source     string
		constraint string
		pinned     bool
	}{
		// git, pinned
		{"git::https://github.com/LEGO/observability-platform.git//applications/istar/infrastructure/modules/tenants?ref=" + sha, "", true},
		{"git::ssh://git@github.com/LEGO/repo.git?ref=" + sha, "", true},
		{"github.com/LEGO/repo//modules/foo?ref=" + sha, "", true},
		{"git@github.com:LEGO/repo.git?ref=" + sha, "", true},
		{"git::https://example.com/repo.git?depth=1&ref=" + sha, "", true},
		{"git::https://example.com/repo.git?ref=" + sha + "0123456789abcdef01234567", "", true}, // sha256 repos
		// git, floating
		{"git::https://github.com/LEGO/repo.git", "", false},
		{"git::https://github.com/LEGO/repo.git?ref=main", "", false},
		{"git::https://github.com/LEGO/repo.git?ref=v1.2.3", "", false},
		{"git::https://github.com/LEGO/repo.git?ref=605beb1", "", false}, // short sha could also be a branch name
		{"git::https://github.com/LEGO/repo.git?ref=" + "605BEB1ED65A00B79EDBCB74FF851D92399F598D", "", false},
		{"github.com/LEGO/repo", "", false},
		// registry
		{"terraform-aws-modules/vpc/aws", "5.19.0", true},
		{"terraform-aws-modules/vpc/aws", "= 5.19.0", true},
		{"terraform-aws-modules/vpc/aws", "v5.19.0", true},
		{"app.terraform.io/org/vpc/aws", "1.0.0", true},
		{"terraform-aws-modules/vpc/aws//modules/vpc-endpoints", "5.19.0", true},
		{"terraform-aws-modules/vpc/aws", "", false},
		{"terraform-aws-modules/vpc/aws", "~> 5.19", false},
		{"terraform-aws-modules/vpc/aws", ">= 5.0.0", false},
		{"terraform-aws-modules/vpc/aws", "5.19.0, < 6.0.0", false},
		{"terraform-aws-modules/vpc/aws", "!= 5.19.0", false},
		// anything else
		{"", "", false},
		{"./local", "", false},
		{"../local", "", false},
		{"/abs/path", "", false},
		{"https://example.com/module.zip", "", false},
		{"http://127.0.0.1:1234/my-module.zip", "", false},
		{"s3::https://s3.amazonaws.com/bucket/module.zip", "", false},
		{"gcs::https://www.googleapis.com/storage/v1/bucket/module", "", false},
		{"hg::http://example.com/repo?ref=" + sha, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.source+"@"+tt.constraint, func(t *testing.T) {
			assert.Equal(t, tt.pinned, IsPinnedModuleSource(tt.source, tt.constraint))
		})
	}
}

func fingerprintWs() *tfreconcilev1alpha1.Workspace {
	return &tfreconcilev1alpha1.Workspace{
		ObjectMeta: v1.ObjectMeta{Name: "ws", Namespace: "ns", Generation: 3},
		Spec: tfreconcilev1alpha1.WorkspaceSpec{
			ToolVersion: "1.13.3",
			Module: &tfreconcilev1alpha1.ModuleSpec{
				Name:   "mod",
				Source: "git::https://github.com/LEGO/repo.git?ref=" + sha,
			},
		},
	}
}

func writeWsFile(t *testing.T, e *Exec, ws *tfreconcilev1alpha1.Workspace, rel, content string) {
	t.Helper()
	p := filepath.Join(e.GetWorkspacePath(ws), rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0644))
}

func installModules(t *testing.T, e *Exec, ws *tfreconcilev1alpha1.Workspace, modulesJSON string, dirs ...string) {
	t.Helper()
	writeWsFile(t, e, ws, ".terraform/modules/modules.json", modulesJSON)
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(e.GetWorkspacePath(ws), d), 0755))
	}
}

const singleModuleJSON = `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"mod","Source":"git::https://github.com/LEGO/repo.git?ref=` + sha + `","Dir":".terraform/modules/mod"}]}`

func TestInitFingerprint(t *testing.T) {
	e := New(t.TempDir())
	ws := fingerprintWs()
	env := map[string]string{"A": "1", "B": "2"}

	_, err := e.InitFingerprint(ws, env)
	require.ErrorIs(t, err, ErrModulesNotInstalled, "main.tf missing")

	writeWsFile(t, e, ws, "main.tf", "module \"mod\" {}")
	_, err = e.InitFingerprint(ws, env)
	require.ErrorIs(t, err, ErrModulesNotInstalled, "modules.json missing")

	installModules(t, e, ws, singleModuleJSON, ".terraform/modules/mod")
	base, err := e.InitFingerprint(ws, env)
	require.NoError(t, err)

	again, err := e.InitFingerprint(ws, map[string]string{"B": "2", "A": "1"})
	require.NoError(t, err)
	assert.Equal(t, base, again, "fingerprint must be stable and independent of map ordering")

	writeWsFile(t, e, ws, ".terraform/modules/mod/.git/index", "changes on every clone")
	writeWsFile(t, e, ws, "plan.out", "plan")
	unchanged, err := e.InitFingerprint(ws, env)
	require.NoError(t, err)
	assert.Equal(t, base, unchanged)

	changes := map[string]func(){
		"env value":   func() { env["A"] = "changed" },
		"env added":   func() { env["C"] = "3" },
		"tool":        func() { ws.Spec.Tool = tfreconcilev1alpha1.ToolOpenTofu },
		"version":     func() { ws.Spec.ToolVersion = "1.14.0" },
		"terraformrc": func() { ws.Spec.TerraformRC = "credentials {}" },
		"main.tf":     func() { writeWsFile(t, e, ws, "main.tf", "module \"mod\" { x = 1 }") },
		"lock file":   func() { writeWsFile(t, e, ws, ".terraform.lock.hcl", "provider {}") },
		"modules":     func() { writeWsFile(t, e, ws, ".terraform/modules/modules.json", `{"Modules":[]}`) },
	}
	prev := base
	for name, change := range changes {
		change()
		fp, err := e.InitFingerprint(ws, env)
		require.NoError(t, err, name)
		assert.NotEqual(t, prev, fp, "fingerprint must change when %s changes", name)
		prev = fp
	}
}

func TestInitFingerprintIndependentOfRootDir(t *testing.T) {
	ws := fingerprintWs()
	fingerprint := func(e *Exec) string {
		writeWsFile(t, e, ws, "main.tf", "module \"mod\" {}")
		installModules(t, e, ws, singleModuleJSON, ".terraform/modules/mod")
		fp, err := e.InitFingerprint(ws, map[string]string{
			"TF_PLUGIN_CACHE_DIR": e.PluginCacheDir,
			"TOKEN_FILE":          filepath.Join(e.GetWorkspacePath(ws), "token"),
		})
		require.NoError(t, err)
		return fp
	}
	assert.Equal(t, fingerprint(New(t.TempDir())), fingerprint(New(t.TempDir())))
}

func TestNestedModulesPinned(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		dirs    []string
		pinned  bool
		errIs   error
		errText string
	}{
		{
			name:   "root module only",
			json:   singleModuleJSON,
			dirs:   []string{".terraform/modules/mod"},
			pinned: true,
		},
		{
			name: "relative children are covered by parent",
			json: `{"Modules":[{"Key":"","Source":"","Dir":"."},
				{"Key":"mod","Source":"git::https://github.com/LEGO/repo.git?ref=` + sha + `","Dir":".terraform/modules/mod"},
				{"Key":"mod.child","Source":"./child","Dir":".terraform/modules/mod/child"},
				{"Key":"mod.child.grand","Source":"../grand","Dir":".terraform/modules/mod/grand"}]}`,
			dirs:   []string{".terraform/modules/mod/child", ".terraform/modules/mod/grand"},
			pinned: true,
		},
		{
			name: "nested pinned git module",
			json: `{"Modules":[{"Key":"","Source":"","Dir":"."},
				{"Key":"mod","Source":"git::https://github.com/LEGO/repo.git?ref=` + sha + `","Dir":".terraform/modules/mod"},
				{"Key":"mod.other","Source":"git::https://github.com/LEGO/other.git?ref=` + sha + `","Dir":".terraform/modules/mod.other"}]}`,
			dirs:   []string{".terraform/modules/mod", ".terraform/modules/mod.other"},
			pinned: true,
		},
		{
			name: "nested floating git module",
			json: `{"Modules":[{"Key":"","Source":"","Dir":"."},
				{"Key":"mod","Source":"git::https://github.com/LEGO/repo.git?ref=` + sha + `","Dir":".terraform/modules/mod"},
				{"Key":"mod.other","Source":"git::https://github.com/LEGO/other.git?ref=main","Dir":".terraform/modules/mod.other"}]}`,
			dirs:   []string{".terraform/modules/mod", ".terraform/modules/mod.other"},
			pinned: false,
		},
		{
			name: "nested registry module, constraint unknown",
			json: `{"Modules":[{"Key":"","Source":"","Dir":"."},
				{"Key":"mod","Source":"git::https://github.com/LEGO/repo.git?ref=` + sha + `","Dir":".terraform/modules/mod"},
				{"Key":"mod.vpc","Source":"registry.terraform.io/terraform-aws-modules/vpc/aws","Version":"5.19.0","Dir":".terraform/modules/mod.vpc"}]}`,
			dirs:   []string{".terraform/modules/mod", ".terraform/modules/mod.vpc"},
			pinned: false,
		},
		{
			name:  "no modules.json",
			errIs: ErrModulesNotInstalled,
		},
		{
			name:  "module dir missing",
			json:  singleModuleJSON,
			errIs: ErrModulesNotInstalled,
		},
		{
			name:    "corrupt modules.json",
			json:    `{`,
			errText: "failed to parse modules.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(t.TempDir())
			ws := fingerprintWs()
			require.NoError(t, e.SetupWorkspace(ws))
			if tt.json != "" {
				installModules(t, e, ws, tt.json, tt.dirs...)
			}

			pinned, err := e.NestedModulesPinned(ws)
			switch {
			case tt.errIs != nil:
				require.ErrorIs(t, err, tt.errIs)
			case tt.errText != "":
				require.ErrorContains(t, err, tt.errText)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.pinned, pinned)
			}
		})
	}
}

func TestInitMarker(t *testing.T) {
	e := New(t.TempDir())
	ws := fingerprintWs()
	require.NoError(t, e.SetupWorkspace(ws))

	m, err := e.ReadInitMarker(ws)
	require.NoError(t, err)
	assert.Nil(t, m)

	require.NoError(t, e.WriteInitMarker(ws, "abc"))
	m, err = e.ReadInitMarker(ws)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "abc", m.Fingerprint)
	assert.Equal(t, int64(3), m.Generation)
	assert.Equal(t, e.BootID, m.BootID)
	assert.NotEmpty(t, e.BootID)
	assert.NotEqual(t, e.BootID, New(t.TempDir()).BootID, "each process start must get a new boot id")

	_, err = os.Stat(filepath.Join(e.GetWorkspacePath(ws), ".terraform", initMarkerFile))
	assert.True(t, os.IsNotExist(err))

	writeWsFile(t, e, ws, initMarkerFile, "not json")
	m, err = e.ReadInitMarker(ws)
	require.NoError(t, err)
	assert.Nil(t, m, "corrupt marker is treated as missing")

	require.NoError(t, e.RemoveInitMarker(ws))
	require.NoError(t, e.RemoveInitMarker(ws), "removing a missing marker is not an error")
	m, err = e.ReadInitMarker(ws)
	require.NoError(t, err)
	assert.Nil(t, m)
}
