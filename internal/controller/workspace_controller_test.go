package controller

import (
	"context"
	"path/filepath"
	"testing"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/internal/testutils"
	"github.com/stretchr/testify/assert"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestWorkspaceController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "crds")},
		BinaryAssetsDirectory: testutils.GetFirstFoundEnvTestBinaryDir(),
		ErrorIfCRDPathMissing: true,
		Scheme:                k8sscheme.Scheme,
	}

	err := tfreconcilev1alpha1.AddToScheme(testEnv.Scheme)
	assert.NoError(t, err)

	cfg, err := testEnv.Start()
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	k8sClient, err := client.New(cfg, client.Options{Scheme: testEnv.Scheme})

	const resourceName = "test-resource"
	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}
	workspace := &tfreconcilev1alpha1.Workspace{}

	t.Run("creating the custom resource for the Kind Workspace", func(t *testing.T) {
		err := k8sClient.Get(ctx, typeNamespacedName, workspace)
		if err != nil && errors.IsNotFound(err) {
			resource := &tfreconcilev1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: tfreconcilev1alpha1.WorkspaceSpec{
					Backend: tfreconcilev1alpha1.BackendSpec{
						Type: "s3",
						Inputs: &apiextensionsv1.JSON{
							Raw: []byte(`{"bucket": "my-bucket"}`),
						},
					},
					ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
						{
							Name:    "aws",
							Version: "1.0",
							Source:  "hashicorp/aws",
						},
					},
					Module: &tfreconcilev1alpha1.ModuleSpec{
						Source:  "terraform-aws-modules/vpc/aws",
						Version: "5.19.0",
					},
				},
			}
			assert.NoError(t, k8sClient.Create(ctx, resource))
		}
	})
}
