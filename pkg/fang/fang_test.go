package fang

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestLoader_SetPath(t *testing.T) {
	t.Run("top level path", func(t *testing.T) {
		// Given
		sut := New[struct {
			Foo string
			Bar string
		}]()

		// When
		sut, err := sut.SetPath("Foo", "test")

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "test", sut.Data.Foo)
		assert.Equal(t, "", sut.Data.Bar)
	})

	t.Run("nested path", func(t *testing.T) {
		// Given
		sut := New[struct {
			Foo struct {
				Baz string
			}
			Bar struct {
				Baz string
			}
		}]()

		// When
		sut, err := sut.SetPath("Foo.Baz", "test")

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "test", sut.Data.Foo.Baz)
		assert.Equal(t, "", sut.Data.Bar.Baz)
	})

	t.Run("int value", func(t *testing.T) {
		// Given
		sut := New[struct {
			Foo int
		}]()

		// When
		sut, err := sut.SetPath("Foo", 42)

		// Then
		assert.NoError(t, err)
		assert.Equal(t, 42, sut.Data.Foo)
	})
}

func TestLoader_Load(t *testing.T) {
	t.Run("returns default when no hooks", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string
		}

		sut := New[dataStruct]().WithDefault(dataStruct{Foo: "Bar"})

		// When
		data, err := sut.Load()

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "Bar", data.Foo)
	})

	t.Run("can override with env hook", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string
		}

		t.Setenv("FOO", "EnvBar")

		sut := New[dataStruct]().
			WithDefault(dataStruct{Foo: "Bar"}).
			WithAutomaticEnv("")

		// When
		data, err := sut.Load()

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "EnvBar", data.Foo)
	})

	t.Run("runs multiple env loaders", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string
			Bar string
		}

		t.Setenv("FOO", "envLoader1")
		t.Setenv("BAR", "envLoader2")

		sut := New[dataStruct]().
			WithDefault(dataStruct{Foo: "Bar"}).
			WithEnvironment(EnvLoader[dataStruct]{Bindings: map[string]string{
				"FOO": "Foo",
			}}).
			WithEnvironment(EnvLoader[dataStruct]{Bindings: map[string]string{
				"BAR": "Bar",
			}})

		// When
		data, err := sut.Load()

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "envLoader1", data.Foo)
		assert.Equal(t, "envLoader2", data.Bar)
	})

	t.Run("second hook overrides first hook", func(t *testing.T) {
		// Given
		type dataStruct struct {
			Foo string
		}

		t.Setenv("FOO", "envLoader1")
		t.Setenv("BAR", "envLoader2")

		sut := New[dataStruct]().
			WithDefault(dataStruct{Foo: "Bar"}).
			WithEnvironment(EnvLoader[dataStruct]{Bindings: map[string]string{
				"FOO": "Foo",
			}}).
			WithEnvironment(EnvLoader[dataStruct]{Bindings: map[string]string{
				"BAR": "Foo",
			}})

		// When
		data, err := sut.Load()

		// Then
		assert.NoError(t, err)
		assert.Equal(t, "envLoader2", data.Foo)
	})
}
