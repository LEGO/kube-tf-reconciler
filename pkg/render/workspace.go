package render

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
)

func addRequiredProviders(body *hclwrite.Body, providers []tfreconcilev1alpha1.Provider) error {
	if len(providers) == 0 {
		return nil
	}

	requiredProvidersBlock := body.AppendNewBlock("required_providers", nil)
	for _, provider := range providers {
		requiredProvidersBlock.Body().SetAttributeValue(provider.Name, cty.ObjectVal(map[string]cty.Value{
			"source":  cty.StringVal(provider.Spec.Source),
			"version": cty.StringVal(provider.Spec.Version),
		}))
	}

	return nil
}

func addBackend(body *hclwrite.Body, backend tfreconcilev1alpha1.BackendSpec) error {
	be := body.AppendNewBlock("backend", []string{backend.Type})
	if backend.Inputs != nil {
		var inputs map[string]interface{}
		err := json.Unmarshal(backend.Inputs.Raw, &inputs)
		if err != nil {
			return fmt.Errorf("could not unmarshal inputs: %w", err)
		}

		// Set each key-value pair as an attribute in the backend block
		for key, value := range inputs {
			switch v := value.(type) {
			case string:
				be.Body().SetAttributeValue(key, cty.StringVal(v))
			case float64:
				be.Body().SetAttributeValue(key, cty.NumberFloatVal(v))
			case bool:
				be.Body().SetAttributeValue(key, cty.BoolVal(v))
			default:
				log.Printf("unsupported type for key %s: %T", key, v)
			}
		}
	}

	return nil
}

func Workspace(body *hclwrite.Body, ws tfreconcilev1alpha1.Workspace, providers []tfreconcilev1alpha1.Provider) error {
	// Create the terraform block
	terraformBlock := body.AppendNewBlock("terraform", nil)
	//terraformBlock.Body().SetAttributeValue("required_version", cty.StringVal(">= 1.7.4"))

	err := addRequiredProviders(terraformBlock.Body(), providers)
	if err != nil {
		return fmt.Errorf("failed to add required providers: %w", err)
	}
	// Set the required_version attribute
	err = addBackend(terraformBlock.Body(), ws.Spec.Backend)
	if err != nil {
		return fmt.Errorf("failed to add backend: %w", err)
	}

	return nil
}
