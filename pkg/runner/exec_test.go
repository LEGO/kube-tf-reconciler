package runner

import (
	"context"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExec_Error(t *testing.T) {
	t.Run("non-existing exec", func(t *testing.T) {
		_, err := NewTerraform("terraform-that-does-not-exist", t.TempDir())
		assert.ErrorIs(t, err, ErrTerraformNotFound)
	})

	t.Run("non-existing dir", func(t *testing.T) {
		tf, err := NewTerraform("terraform", "./non-existing")
		assert.NoError(t, err)
		err = tf.Init(context.Background())
		assert.ErrorIs(t, err, ErrRunning)
	})

	t.Run("non-existing config", func(t *testing.T) {
		tf, err := NewTerraform("terraform", t.TempDir())
		assert.NoError(t, err)
		_, err = tf.Apply(context.Background())
		assert.ErrorIs(t, err, ErrRunning)
	})
}

func TestExec_Success(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		tf, err := NewTerraform("terraform", t.TempDir())
		assert.NoError(t, err)
		v, err := tf.Version(context.Background())
		assert.NoError(t, err)
		assert.NotEmpty(t, v.TerraformVersion)
	})
	t.Run("init", func(t *testing.T) {
		tf, err := NewTerraform("terraform", t.TempDir())
		assert.NoError(t, err)
		err = tf.Init(context.Background())
		assert.NoError(t, err)
	})
	t.Run("plan", func(t *testing.T) {
		tf, err := NewTerraform("terraform", t.TempDir())
		assert.NoError(t, err)

		err = os.WriteFile(path.Join(tf.wd, "main.tf"), []byte(`
locals {
	test = "hey"
}`), 0644)

		err = tf.Init(context.Background())
		assert.NoError(t, err)

		_, err = tf.Plan(context.Background())
		assert.NoError(t, err)
	})

	t.Run("apply", func(t *testing.T) {
		tf, err := NewTerraform("terraform", t.TempDir())
		assert.NoError(t, err)

		err = os.WriteFile(path.Join(tf.wd, "main.tf"), []byte(`
locals {
	test = "hey"
}`), 0644)

		err = tf.Init(context.Background())
		assert.NoError(t, err)

		outputs, err := tf.Apply(context.Background())
		assert.NoError(t, err)
		assert.NotEmpty(t, outputs)
	})

}
