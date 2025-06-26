package fang

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestEnvLoader_AutomaticEnv(t *testing.T) {
	t.Run("load simple env", func(t *testing.T) {
		// Given
		envLoader := EnvLoader[struct{ Foo string }]{
			Bindings: map[string]string{},
		}

		// When
		envLoader = envLoader.AutomaticEnv()

		// Then
		assert.Equal(t, "Foo", envLoader.Bindings["FOO"])
	})
	t.Run("uses fang struct tag", func(t *testing.T) {
		// Given
		envLoader := EnvLoader[struct {
			Foo string `fang:"bar"`
		}]{
			Bindings: map[string]string{},
		}

		// When
		envLoader = envLoader.AutomaticEnv()

		// Then
		assert.Equal(t, "Foo", envLoader.Bindings["BAR"])
	})
	t.Run("uses mapstructure struct tag", func(t *testing.T) {
		// Given
		envLoader := EnvLoader[struct {
			Foo string `mapstructure:"bar"`
		}]{
			Bindings: map[string]string{},
		}

		// When
		envLoader = envLoader.AutomaticEnv()

		// Then
		assert.Equal(t, "Foo", envLoader.Bindings["BAR"])
	})
	t.Run("handle mapstructure tag with options", func(t *testing.T) {
		// Given
		envLoader := EnvLoader[struct {
			Foo string `mapstructure:"bar,omitempty"`
		}]{
			Bindings: map[string]string{},
		}

		// When
		envLoader = envLoader.AutomaticEnv()

		// Then
		assert.Equal(t, "Foo", envLoader.Bindings["BAR"])
	})
	t.Run("handle nested mapstructure tags", func(t *testing.T) {
		// Given
		envLoader := EnvLoader[struct {
			Foo struct {
				Bar string `mapstructure:"baa"`
			} `mapstructure:"fuu,omitempty"`
		}]{
			Bindings: map[string]string{},
		}

		// When
		envLoader = envLoader.AutomaticEnv()

		// Then
		assert.Equal(t, "Foo.Bar", envLoader.Bindings["FUU__BAA"])
	})
	t.Run("handle struct tag inside untagged field", func(t *testing.T) {
		// Given
		envLoader := EnvLoader[struct {
			Foo struct {
				Bar string `mapstructure:"baa"`
			}
		}]{
			Bindings: map[string]string{},
		}

		// When
		envLoader = envLoader.AutomaticEnv()

		// Then
		assert.Equal(t, "Foo.Bar", envLoader.Bindings["FOO__BAA"])
	})
}

func TestEnvLoader_Loader(t *testing.T) {
	t.Run("loads unnested env", func(t *testing.T) {
		// Given
		t.Setenv("FOO", "Bar")

		type data struct {
			Foo string
		}

		envLoader := EnvLoader[data]{
			Bindings: map[string]string{
				"FOO": "Foo",
			},
		}

		// When
		l, err := envLoader.Hook(Loader[data]{})
		assert.NoError(t, err)
		assert.Equal(t, "Bar", l.Data.Foo)
	})
	t.Run("loads nested env", func(t *testing.T) {
		// Given
		t.Setenv("FOO__BAR", "Baz")

		type data struct {
			Foo struct {
				Bar string
			}
		}

		envLoader := EnvLoader[data]{
			Bindings: map[string]string{
				"FOO__BAR": "Foo.Bar",
			},
		}

		// When
		l, err := envLoader.Hook(Loader[data]{})
		assert.NoError(t, err)
		assert.Equal(t, "Baz", l.Data.Foo.Bar)
	})
	t.Run("overrides defaults when set", func(t *testing.T) {
		// Given
		t.Setenv("FOO__BAR", "Baz")

		type data struct {
			Foo struct {
				Bar string
			}
		}

		envLoader := EnvLoader[data]{
			Bindings: map[string]string{
				"FOO__BAR": "Foo.Bar",
			},
		}

		// When
		l, err := envLoader.Hook(Loader[data]{Data: data{Foo: struct{ Bar string }{Bar: "Baz"}}})
		assert.NoError(t, err)
		assert.Equal(t, "Baz", l.Data.Foo.Bar)
	})
	t.Run("respects defaults when nothing set", func(t *testing.T) {
		// Given
		type data struct {
			Foo struct {
				Bar string
			}
		}

		envLoader := EnvLoader[data]{
			Bindings: map[string]string{
				"FOO__BAR": "Foo.Bar",
			},
		}

		// When
		l, err := envLoader.Hook(Loader[data]{Data: data{Foo: struct{ Bar string }{Bar: "Baz"}}})
		assert.NoError(t, err)
		assert.Equal(t, "Baz", l.Data.Foo.Bar)
	})
	t.Run("respects defaults when sibling set", func(t *testing.T) {
		// Given
		t.Setenv("FOO__BAR", "FooBar")

		type data struct {
			Foo struct {
				Bar string
				Baz string
			}
		}

		envLoader := EnvLoader[data]{
			Bindings: map[string]string{
				"FOO__BAR": "Foo.Bar",
				"FOO__BAZ": "Foo.Baz",
			},
		}

		// When
		l, err := envLoader.Hook(Loader[data]{Data: data{Foo: struct {
			Bar string
			Baz string
		}{Bar: "default", Baz: "default"}}})
		assert.NoError(t, err)
		assert.Equal(t, "FooBar", l.Data.Foo.Bar)
		assert.Equal(t, "default", l.Data.Foo.Baz)
	})
}
