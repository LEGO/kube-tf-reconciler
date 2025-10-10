package testutils

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunningModuleHost_Empty(t *testing.T) {
	m, shutdown := NewModuleHost()
	t.Cleanup(shutdown)
	c := m.server.Client()
	resp, err := c.Get(m.URL() + "/terraform.zip")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestGettingModule_success(t *testing.T) {
	m, shutdown := NewModuleHost()
	t.Cleanup(shutdown)
	c := m.server.Client()
	m.AddFileToModule("terraform", "main.tf", `resource "null_resource" "example" {}`)

	resp, err := c.Get(m.URL() + "/terraform.zip")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

}
