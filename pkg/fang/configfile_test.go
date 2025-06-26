package fang

import (
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFileLoader_GetExtensions(t *testing.T) {
	t.Run("yaml extensions", func(t *testing.T) {
		// Given
		l := ConfigFileLoader[any]{ConfigFileOptions{Type: ConfigFileTypeYaml}}

		// When
		extensions := l.GetExtensions()

		// Then
		assert.Equal(t, extensions, []string{".yaml", ".yml"})
	})
	t.Run("extensions overridable", func(t *testing.T) {
		// Given
		l := ConfigFileLoader[any]{ConfigFileOptions{Type: ConfigFileTypeYaml, Extensions: []string{".foo"}}}

		// When
		extensions := l.GetExtensions()

		// Then
		assert.Equal(t, extensions, []string{".foo"})
	})
}

func TestConfigFileLoader_IsFileDiscoverable(t *testing.T) {
	t.Run("when yaml file name and extension matches", func(t *testing.T) {
		// Given
		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		isDiscoverable := l.IsFileDiscoverable("config.yaml")

		// Then
		assert.True(t, isDiscoverable)
	})
	t.Run("not when yaml file name does not match", func(t *testing.T) {
		// Given
		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		isDiscoverable := l.IsFileDiscoverable("kongfig.yaml")

		// Then
		assert.False(t, isDiscoverable)
	})
	t.Run("not when yaml file extension does not match", func(t *testing.T) {
		// Given
		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		isDiscoverable := l.IsFileDiscoverable("config.foo")

		// Then
		assert.False(t, isDiscoverable)
	})
	t.Run("not when file without extension", func(t *testing.T) {
		// Given
		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		isDiscoverable := l.IsFileDiscoverable("config")

		// Then
		assert.False(t, isDiscoverable)
	})
}

func TestConfigFileLoader_FindConfigFilesInPath(t *testing.T) {
	t.Run("finds single config file in path", func(t *testing.T) {
		// Given
		dir := t.TempDir()
		createYamlFile(t, dir, "config.yaml", "foo")
		createYamlFile(t, dir, "not-config.yaml", "foo")
		createYamlFile(t, dir, "not-config.txt", "foo")

		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		files := l.FindConfigFilesInPath(dir)

		// Then
		if assert.Len(t, files, 1) {
			assert.Equal(t, filepath.Join(dir, "config.yaml"), files[0])
		}
	})

	t.Run("finds multiple config files in path", func(t *testing.T) {
		// Given
		dir := t.TempDir()
		createYamlFile(t, dir, "config.yaml", "foo")
		createYamlFile(t, dir, "config.yml", "foobar")

		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		files := l.FindConfigFilesInPath(dir)

		// Then
		if assert.Len(t, files, 2) {
			assert.Equal(t, filepath.Join(dir, "config.yaml"), files[0])
			assert.Equal(t, filepath.Join(dir, "config.yml"), files[1])
		}
	})

	t.Run("finds file in $HOME", func(t *testing.T) {
		// Given
		dir := t.TempDir()

		t.Setenv("HOME", dir)
		// For Windows
		t.Setenv("USERPROFILE", dir)
		// For plan9
		t.Setenv("home", dir)

		createYamlFile(t, dir, "config.yaml", "foo")

		l := ConfigFileLoader[any]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
		}}

		// When
		files := l.FindConfigFilesInPath("$HOME")

		// Then
		if assert.Len(t, files, 1) {
			assert.Equal(t, filepath.Join(dir, "config.yaml"), files[0])
		}
	})
}

func TestConfigFileLoader_LoadFile(t *testing.T) {
	t.Run("loads and unmarshals yaml file", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string
		}

		dir := t.TempDir()
		createYamlFile(t, dir, "config.yaml", dataStruct{Foo: "Bar"})
		l := ConfigFileLoader[dataStruct]{ConfigFileOptions{
			Type: ConfigFileTypeYaml,
		}}

		// When
		var data dataStruct
		err := l.LoadFile(filepath.Join(dir, "config.yaml"), &data)

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "Bar", data.Foo)
	})
}

func TestConfigFileLoader_Loader(t *testing.T) {
	t.Run("loads and unmarshals yaml file", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string
		}

		dir := t.TempDir()
		createYamlFile(t, dir, "config.yaml", dataStruct{Foo: "Bar"})
		l := ConfigFileLoader[dataStruct]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
			Paths: []string{dir},
		}}

		// When
		loader, err := l.Hook(Loader[dataStruct]{})

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "Bar", loader.Data.Foo)
	})

	t.Run("does not override default when not set", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string `yaml:"foo"`
		}

		dir := t.TempDir()
		createYamlFile(t, dir, "config.yaml", struct{}{})
		l := ConfigFileLoader[dataStruct]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
			Paths: []string{dir},
		}}

		// When
		loader, err := l.Hook(Loader[dataStruct]{Data: dataStruct{Foo: "default"}})

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "default", loader.Data.Foo)
	})

	t.Run("overrides default when omitempty is true and field is empty", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string `yaml:"foo,omitempty"`
		}

		dir := t.TempDir()
		createYamlFile(t, dir, "config.yaml", dataStruct{})
		l := ConfigFileLoader[dataStruct]{ConfigFileOptions{
			Type:  ConfigFileTypeYaml,
			Names: []string{"config"},
			Paths: []string{dir},
		}}

		// When
		loader, err := l.Hook(Loader[dataStruct]{Data: dataStruct{Foo: "default"}})

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "default", loader.Data.Foo)
	})
}

func createYamlFile(t *testing.T, dir string, name string, content any) {
	bytes, err := yaml.Marshal(content)
	if err != nil {
		assert.FailNow(t, err.Error())
	}
	err = os.WriteFile(filepath.Join(dir, name), bytes, 0644)
}
