package render

import (
	"testing"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/assert"
)

func TestProvider(t *testing.T) {
	p := tfreconcilev1alpha1.ProviderSpec{
		Version: "= 5.40.0",
		Name:    "aws",
		Source:  "hashicorp/aws",
	}

	file := hclwrite.NewEmptyFile()
	err := Provider(file.Body(), p)
	assert.NoError(t, err)
	expected := `provider "aws" {
}
`
	assert.Equal(t, expected, string(file.Bytes()))
}
