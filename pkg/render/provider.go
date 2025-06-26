package render

import (
	"fmt"

	"github.com/hashicorp/hcl/v2/hclwrite"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
)

func Providers(body *hclwrite.Body, providers []tfreconcilev1alpha1.ProviderSpec) error {
	for _, p := range providers {
		err := Provider(body, p)
		if err != nil {
			return fmt.Errorf("failed to render provider %s: %w", p.Name, err)
		}
	}

	return nil
}

func Provider(body *hclwrite.Body, p tfreconcilev1alpha1.ProviderSpec) error {
	body.AppendNewBlock("provider", []string{p.Name})
	return nil
}
