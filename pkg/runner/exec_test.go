package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/stretchr/testify/require"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestChecksum(t *testing.T) {
	dir := t.TempDir()
	e := New(dir)
	ws := &tfreconcilev1alpha1.Workspace{
		ObjectMeta: v1.ObjectMeta{
			Name:      "workspace1",
			Namespace: "test-ns1",
		},
	}
	path := filepath.Join(dir, "workspaces", ws.Namespace, ws.Name, ".terraform", "f1.txt")
	err := os.MkdirAll(filepath.Dir(path), 0755)
	assert.NoError(t, err)
	err = os.WriteFile(path, []byte("file1"), 0644)

	check, err := e.CalculateChecksum(ws)
	assert.NoError(t, err)
	assert.Equal(t, "c147efcfc2d7ea666a9e4f5187b115c90903f0fc896a56df9a6ef5d8f3fc9f31", check)
}

func TestWithOutputStream(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	e := New(dir)
	terraformBinary, err := e.getTerraformBinary(ctx, "1.11.2")
	require.NoError(t, err)
	tf, err := tfexec.NewTerraform(dir, terraformBinary)
	require.NoError(t, err)
	var body string
	WithOutputStream(ctx, tf, func() {
		_, _, err := tf.Version(ctx, false)
		require.NoError(t, err)
		_, _, err = tf.Version(ctx, false)
		require.NoError(t, err)
	}, func(stdout, stderr string) {
		assert.NotEqual(t, body, stdout)
		assert.Truef(t, strings.HasPrefix(stdout, body), "received a different stdout than what we've had so far")
	})
}

func TestMultipleVersions(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	e := New(dir)

	terraformBinary, err := e.getTerraformBinary(ctx, "1.11.2")
	require.NoError(t, err)
	tf, err := tfexec.NewTerraform(dir, terraformBinary)
	require.NoError(t, err)
	version, _, err := tf.Version(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, "1.11.2", version.String())

	terraformBinary, err = e.getTerraformBinary(ctx, "1.14.3")
	require.NoError(t, err)
	tf, err = tfexec.NewTerraform(dir, terraformBinary)
	require.NoError(t, err)
	version, _, err = tf.Version(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, "1.14.3", version.String())
}
