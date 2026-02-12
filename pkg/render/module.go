package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

func Module(body *hclwrite.Body, m *tfreconcilev1alpha1.ModuleSpec) error {
	// Create the module block
	moduleBlock := body.AppendNewBlock("module", []string{m.Name})
	// Set the source attribute
	moduleBlock.Body().SetAttributeValue("source", cty.StringVal(m.Source))

	if m.Version != "" {
		moduleBlock.Body().SetAttributeValue("version", cty.StringVal(m.Version))
	}

	if m.Inputs != nil {
		var inputs map[string]interface{}
		err := json.Unmarshal(m.Inputs.Raw, &inputs)
		if err != nil {
			return fmt.Errorf("failed to unmarshal inputs: %w", err)
		}

		// Map the inputs to the module body
		err = mapInputsToModuleBody(moduleBlock.Body(), inputs)
		if err != nil {
			return fmt.Errorf("failed to map inputs to module: %w", err)
		}
	}

	return nil
}

func mapInputsToModuleBody(body *hclwrite.Body, inputs map[string]interface{}) error {
	keys := slices.Collect(maps.Keys(inputs))
	sort.Strings(keys)
	for _, key := range keys {
		value, err := convertToCtyValue(inputs[key])
		if err != nil {
			return fmt.Errorf("failed to convert %s value to cty: %w", key, err)
		}
		if !value.IsNull() {
			body.SetAttributeValue(key, value)
		}
	}

	return nil
}

func convertToCtyValue(value interface{}) (cty.Value, error) {
	switch v := value.(type) {
	case string:
		return cty.StringVal(v), nil
	case float64:
		return cty.NumberFloatVal(v), nil
	case bool:
		return cty.BoolVal(v), nil
	case map[string]interface{}:
		m := map[string]cty.Value{}
		var err, convErr error
		for key, val := range v {
			m[key], convErr = convertToCtyValue(val)
			if err != nil {
				err = errors.Join(err, fmt.Errorf("failed to convert %s value to cty: %w", key, convErr))
			}
		}
		return cty.ObjectVal(m), err
	case []interface{}:
		if len(v) == 0 {
			return cty.ListValEmpty(cty.DynamicPseudoType), nil
		}
		var list []cty.Value
		var err error
		for idx, item := range v {
			cv, convErr := convertToCtyValue(item)
			if convErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to convert %d value to cty: %w", idx, convErr))
			}
			list = append(list, cv)
		}
		if !cty.CanListVal(list) {
			return cty.ListValEmpty(cty.DynamicPseudoType), errors.Join(err, fmt.Errorf("inconsistent list value types"))
		}
		return cty.ListVal(list), err
	default:
		return cty.NilVal, errors.New(fmt.Sprintf("unexpected value type %T", value))
	}
}
