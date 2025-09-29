package testutils

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
)

func ModuleHost(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)

	files := map[string]string{
		"main.tf":      `resource "null_resource" "example" {}`,
		"variables.tf": `variable "example" { type = string }`,
		"outputs.tf":   `output "example" { value = "hello" }`,
	}

	for name, content := range files {
		f, _ := zipWriter.Create(name)
		f.Write([]byte(content))
	}
	zipWriter.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=terraform.zip")
	io.Copy(w, &buf)
}
