package fang

import (
	"errors"
	"k8s.io/apimachinery/pkg/util/yaml"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type ConfigFileType string

const (
	ConfigFileTypeYaml ConfigFileType = "yaml"
)

type ConfigFileOptions struct {
	Type       ConfigFileType
	Names      []string
	Paths      []string
	Extensions []string
}

type ConfigFileLoader[T any] struct {
	Options ConfigFileOptions
}

func (t ConfigFileLoader[T]) Hook(l Loader[T]) (Loader[T], error) {
	var err error
	for _, path := range t.Options.Paths {
		files := t.FindConfigFilesInPath(path)
		for _, file := range files {
			loadErr := t.LoadFile(file, &l.Data)
			if loadErr != nil {
				err = errors.Join(err, loadErr)
			}
		}
	}

	return l, nil
}

func (t ConfigFileLoader[T]) FindConfigFilesInPath(path string) []string {
	homeDir, err := os.UserHomeDir()
	if err == nil {
		path = strings.ReplaceAll(path, "$HOME", homeDir)
	}

	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}

	var files []string
	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		if t.IsFileDiscoverable(entry.Name()) {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}

	return files
}

func (t ConfigFileLoader[T]) LoadFile(file string, data *T) error {
	bytes, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	switch t.Options.Type {
	case ConfigFileTypeYaml:
		err = yaml.Unmarshal(bytes, data)
		return err
	}
	return errors.New("invalid file type")
}

func (t ConfigFileLoader[T]) IsFileDiscoverable(filePath string) bool {
	basename := filepath.Base(filePath)
	basename = strings.TrimSuffix(basename, filepath.Ext(basename))
	if !slices.Contains(t.Options.Names, basename) {
		return false
	}
	extensions := t.GetExtensions()
	if !slices.Contains(extensions, filepath.Ext(filePath)) {
		return false
	}
	return true
}

func (t ConfigFileLoader[T]) GetExtensions() []string {
	if t.Options.Extensions != nil {
		return t.Options.Extensions
	}
	switch t.Options.Type {
	case ConfigFileTypeYaml:
		return []string{".yaml", ".yml"}
	}

	return nil
}
