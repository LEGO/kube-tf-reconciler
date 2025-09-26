package render

import (
	"testing"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRenderToFilesystem(t *testing.T) {
	r := NewFileRender(t.TempDir())

	res, err := r.Render(tfreconcilev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workspace",
			Namespace: "default",
		},
		Spec: tfreconcilev1alpha1.WorkspaceSpec{
			Backend: tfreconcilev1alpha1.BackendSpec{
				Type: "local",
			},
			Module: &tfreconcilev1alpha1.ModuleSpec{
				Name:    "my-module",
				Source:  "terraform-aws-modules/vpc/aws",
				Version: "5.19.0",
			},
			ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
				{
					Name:    "aws",
					Version: "1.0",
					Source:  "hashicorp/aws",
				},
			},
		},
	})
	assert.NoError(t, err)

	expectedRender := `terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "1.0"
    }
  }
  backend "local" {
  }
}
provider "aws" {
}
module "my-module" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.19.0"
}
`

	assert.Equal(t, expectedRender, res)
}

func TestRenderLocalModule(t *testing.T) {

	localModule := `resource "random_pet" "name" {
  length    = 2
  separator = "-"
}
`

	r := NewLocalModuleRenderer(t.TempDir(), map[string][]byte{
		"my-test-module": []byte(localModule),
	})

	res, err := r.Render(tfreconcilev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workspace",
			Namespace: "default",
		},
		Spec: tfreconcilev1alpha1.WorkspaceSpec{
			Backend: tfreconcilev1alpha1.BackendSpec{
				Type: "local",
			},
			Module: &tfreconcilev1alpha1.ModuleSpec{
				Name:   "my-module",
				Source: "my-test-module",
			},
			ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
				{
					Name:    "aws",
					Version: "1.0",
					Source:  "hashicorp/aws",
				},
			},
		},
	})
	assert.NoError(t, err)
	assert.Contains(t, res, r.tmpPath)
}
