package runner

import (
	"os"
	"path/filepath"
	"testing"

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
