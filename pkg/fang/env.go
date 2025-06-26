package fang

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"unicode"
)

type EnvLoader[T any] struct {
	Bindings  map[string]string
	EnvPrefix string
}

func (e EnvLoader[T]) Hook(l Loader[T]) (Loader[T], error) {
	var err error
	for env, fieldKey := range e.Bindings {
		if envValue := os.Getenv(env); envValue != "" {
			var setPathErr error
			l, setPathErr = l.SetPath(fieldKey, envValue)
			if setPathErr != nil {
				err = errors.Join(err, setPathErr)
			}
		}
	}
	return l, err
}

func (e EnvLoader[T]) AutomaticEnv() EnvLoader[T] {
	return e.automaticEnv(reflect.TypeFor[T](), nil, nil)
}

func (e EnvLoader[T]) automaticEnv(configType reflect.Type, parentAliases, parentKeys []string) EnvLoader[T] {
	for idx := 0; idx < configType.NumField(); idx++ {
		field := configType.Field(idx)

		var tag string
		for _, t := range []string{"fang", "mapstructure"} {
			tag = field.Tag.Get(t)
			if tag != "" {
				break
			}
		}

		if tag == "" {
			tag = field.Name
		} else {
			tag = strings.Split(tag, ",")[0]
		}

		alias := ""
		for c := 0; c < len(tag); c++ {
			if c > 0 && unicode.IsUpper(rune(tag[c])) {
				alias += "_" + string(unicode.ToLower(rune(tag[c])))
			} else {
				alias += string(tag[c])
			}
		}
		alias = strings.NewReplacer("-", "_", ".", "__").Replace(alias)

		if field.Type.Kind() == reflect.Struct {
			e.automaticEnv(field.Type, append(parentAliases, alias), append(parentKeys, field.Name))
		} else {
			envPrefix := e.EnvPrefix
			if envPrefix != "" {
				envPrefix += "_"
			}
			aliasKey := strings.ToUpper(envPrefix + strings.Join(append(parentAliases, alias), "__"))
			e.Bindings[aliasKey] = strings.Join(append(parentKeys, field.Name), ".")
		}
	}

	return e
}
