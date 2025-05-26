package render

import (
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
)

func Module(body *hclwrite.Body, m tfreconcilev1alpha1.Module) error {
	// Create the module block
	moduleBlock := body.AppendNewBlock("module", []string{m.Name})
	// Set the source attribute
	moduleBlock.Body().SetAttributeValue("source", cty.StringVal(m.Spec.Source))

	return nil
}
