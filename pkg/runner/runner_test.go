package runner

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunner_Success(t *testing.T) {
	tf, err := NewTerraform("terraform", t.TempDir())
	assert.NoError(t, err)

	sut := Runner{
		tf: tf,
		providerConfigs: []string{
			`
terraform {
  required_providers {
    random = {
      source = "hashicorp/random"
      version = "3.7.1"
    }
  }
}

provider "random" {
  # Configuration options
}
`,
		},
		moduleConfigs: []string{
			`
locals {
	prefix = "prefix_name"
}
resource "random_pet" "server" {}

output "name" {
	value = random_pet.server.id
}
output "prefix" {
	value = local.prefix
}
`,
		},
	}

	err = sut.Init(context.Background())
	assert.NoError(t, err)

	outputs, err := sut.Execute(context.Background())
	assert.NoError(t, err)
	assert.NotEmpty(t, outputs)
	assert.Len(t, outputs, 2)
	assert.NotEmpty(t, sut.ApplyResult)
	assert.Empty(t, sut.PlanResult)
	assert.ElementsMatch(t, []string{"name", "prefix"}, slices.Collect(maps.Keys(outputs)))
}
