package render

import (
	"testing"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/assert"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
	"lukaspj.io/kube-tf-reconciler/internal/testutils"
)

func TestModuleSuccess(t *testing.T) {
	f := hclwrite.NewEmptyFile()

	expectedWs := `module "my-module" {
  source = "./my-module"
  input  = ["test", "test2"]
  something = {
    bool = true
  }
  test = "test"
}
`

	err := Module(f.Body(), &tfreconcilev1alpha1.ModuleSpec{
		Source: "./my-module",
		Name:   "my-module",
		Inputs: testutils.Json(map[string]interface{}{
			"test":  "test",
			"input": []string{"test", "test2"},
			"something": map[string]interface{}{
				"bool": true,
			},
		}),
	})
	assert.NoError(t, err)
	assert.Equal(t, expectedWs, string(f.Bytes()))
}
