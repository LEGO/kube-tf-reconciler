package runner

import (
	"github.com/hashicorp/hcl/v2/hclwrite"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
	"lukaspj.io/kube-tf-reconciler/pkg/render"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Planner(c client.Client, ws tfreconcilev1alpha1.Workspace) error {
	f := hclwrite.NewEmptyFile()

	//c.Get()
	err := render.Workspace(f.Body(), ws, []tfreconcilev1alpha1.Provider{})
	if err != nil {
		return err
	}

	print(f.Bytes())

	return nil
}
