package render

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
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
		mapInputsToModuleBody(moduleBlock.Body(), inputs)
	}

	return nil
}

func mapInputsToModuleBody(body *hclwrite.Body, inputs map[string]interface{}) {
	keys := slices.Collect(maps.Keys(inputs))
	sort.Strings(keys)
	for _, key := range keys {
		value := convertToCtyValue(inputs[key])
		if !value.IsNull() {
			body.SetAttributeValue(key, value)
		}
	}
}

func convertToCtyValue(value interface{}) cty.Value {
	switch v := value.(type) {
	case string:
		return cty.StringVal(v)
	case float64:
		return cty.NumberFloatVal(v)
	case bool:
		return cty.BoolVal(v)
	case map[string]interface{}:
		m := map[string]cty.Value{}
		for key, val := range v {
			m[key] = convertToCtyValue(val)
		}
		return cty.ObjectVal(m)
	case []interface{}:
		var list []cty.Value
		for _, item := range v {
			list = append(list, convertToCtyValue(item))
		}
		return cty.ListVal(list)
	default:
		return cty.NilVal
	}
}
