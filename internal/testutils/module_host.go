package testutils

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
)

type ModuleHost struct {
	moduleFiles map[string]map[string]string
	server      *httptest.Server
}

func NewModuleHost() (*ModuleHost, func()) {
	m := &ModuleHost{
		moduleFiles: make(map[string]map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/{module}", m.ServeHTTP)
	s := httptest.NewServer(mux)
	m.server = s

	return m, s.Close
}

func (m *ModuleHost) URL() string {
	return m.server.URL
}

func (m *ModuleHost) AddFileToModule(name, file, content string) {
	module, ok := m.moduleFiles[name]
	if !ok {
		module = make(map[string]string)
	}
	module[file] = content
	m.moduleFiles[name] = module
}

func (m *ModuleHost) ModuleSource(name string) string {
	return fmt.Sprintf("%s/%s.zip", m.server.URL, name)
}

func (m *ModuleHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	fileNameExt := r.PathValue("module")
	ext := filepath.Ext(fileNameExt)
	if ext != ".zip" {
		http.Error(w, "Not a zip file", http.StatusNotFound)
	}
	fileName := fileNameExt[:len(fileNameExt)-len(ext)]
	moduleFiles, ok := m.moduleFiles[fileName]
	if !ok {
		http.NotFound(w, r)
		return
	}

	for name, content := range moduleFiles {
		f, _ := zipWriter.Create(name)
		f.Write([]byte(content))
	}
	zipWriter.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileNameExt))
	io.Copy(w, &buf)
}
