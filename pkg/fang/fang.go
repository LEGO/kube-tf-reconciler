package fang

import (
	"errors"
	"reflect"
	"strings"
)

type Loader[T any] struct {
	Data  T
	Fangs []func(Loader[T]) (Loader[T], error)
}

func New[T any]() Loader[T] {
	return Loader[T]{}
}

func (l Loader[T]) WithDefault(value T) Loader[T] {
	l.Data = value
	return l
}

func (l Loader[T]) WithAutomaticEnv(envPrefix string) Loader[T] {
	l.Fangs = append(l.Fangs, EnvLoader[T]{
		Bindings:  map[string]string{},
		EnvPrefix: envPrefix,
	}.AutomaticEnv().Hook)
	return l
}

func (l Loader[T]) WithEnvironment(e EnvLoader[T]) Loader[T] {
	l.Fangs = append(l.Fangs, e.Hook)
	return l
}

func (l Loader[T]) WithConfigFile(opts ConfigFileOptions) Loader[T] {
	l.Fangs = append(l.Fangs, ConfigFileLoader[T]{
		Options: opts,
	}.Hook)
	return l
}

func (l Loader[T]) SetPath(key string, value any) (Loader[T], error) {
	refVal, err := setPath(reflect.ValueOf(&l.Data), key, value)
	if err == nil {
		l.Data = refVal.Interface().(T)
	}

	return l, err
}

func (l Loader[T]) Load() (T, error) {
	for _, fang := range l.Fangs {
		var err error
		l, err = fang(l)
		if err != nil {
			return l.Data, err
		}
	}

	return l.Data, nil
}

var ErrFieldNotFound = errors.New("field not found")

func setPath(data reflect.Value, key string, value any) (reflect.Value, error) {
	if data.Kind() == reflect.Ptr {
		data = reflect.Indirect(data)
	}

	before, after, found := strings.Cut(key, ".")

	field := data.FieldByName(before)
	if !field.IsValid() {
		return reflect.Value{}, ErrFieldNotFound
	}

	if found {
		refVal, err := setPath(field, after, value)
		if err != nil {
			return reflect.Value{}, err
		}
		field.Set(refVal)
	} else {
		field.Set(reflect.ValueOf(value))
	}
	return data, nil
}
