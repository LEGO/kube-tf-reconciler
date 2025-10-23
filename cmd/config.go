package cmd

import (
	"github.com/LEGO/kube-tf-reconciler/internal/controller"
	"github.com/LEGO/kube-tf-reconciler/pkg/fang"
)

func ConfigFromEnvironment() (controller.Config, error) {
	return ConfigFromEnvironmentWithPrefix("KREC")
}

func ConfigFromEnvironmentWithPrefix(envPrefix string) (controller.Config, error) {
	loader := fang.New[controller.Config]().
		WithDefault(controller.DefaultConfig()).
		WithAutomaticEnv(envPrefix).
		WithConfigFile(fang.ConfigFileOptions{
			Paths: []string{"$HOME", "."},
			Names: []string{"config"},
			Type:  fang.ConfigFileTypeYaml,
		})

	return loader.Load()
}
