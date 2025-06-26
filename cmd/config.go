package cmd

import (
	"github.com/LEGO/kube-tf-reconciler/pkg/fang"
	"github.com/LEGO/kube-tf-reconciler/pkg/operator"
)

func ConfigFromEnvironment() (operator.Config, error) {
	return ConfigFromEnvironmentWithPrefix("KREC")
}

func ConfigFromEnvironmentWithPrefix(envPrefix string) (operator.Config, error) {
	loader := fang.New[operator.Config]().
		WithDefault(operator.DefaultConfig()).
		WithAutomaticEnv(envPrefix).
		WithConfigFile(fang.ConfigFileOptions{
			Paths: []string{"$HOME", "."},
			Names: []string{"config"},
			Type:  fang.ConfigFileTypeYaml,
		})

	return loader.Load()
}
