package cmd

import (
	"reflect"
	"strconv"

	"github.com/LEGO/kube-tf-reconciler/internal/controller"
	"github.com/lukaspj/go-fang"
)

func ConfigFromEnvironment() (controller.Config, error) {
	return ConfigFromEnvironmentWithPrefix("KREC")
}

func ConfigFromEnvironmentWithPrefix(envPrefix string) (controller.Config, error) {
	loader := fang.New[controller.Config]().
		WithDefault(controller.DefaultConfig()).
		WithAutomaticEnv(envPrefix).
		WithMappers(func(from, to reflect.Type, data any) (any, error) {
			if from.Kind() != reflect.String {
				return data, nil
			}
			if to.Kind() != reflect.Bool {
				return data, nil
			}

			return strconv.ParseBool(data.(string))
		}).
		WithConfigFile(fang.ConfigFileOptions{
			Paths: []string{"$HOME", "."},
			Names: []string{"config"},
			Type:  fang.ConfigFileTypeYaml,
		})

	return loader.Load()
}
