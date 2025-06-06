package controller

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
	"lukaspj.io/kube-tf-reconciler/internal/testutils"
	"lukaspj.io/kube-tf-reconciler/pkg/runner"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func TestReconciler(t *testing.T) {
	// Given
	scheme := runtime.NewScheme()
	name := types.NamespacedName{Namespace: "test", Name: "test-ws"}
	require.NoError(t, tfreconcilev1alpha1.AddToScheme(scheme))
	obj := &tfreconcilev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name.Name,
			Namespace:  name.Namespace,
			Generation: 1,
		},
		Spec: tfreconcilev1alpha1.WorkspaceSpec{
			TerraformVersion: "1.12.1",
			TFExec: &tfreconcilev1alpha1.TFSpec{
				Env: []tfreconcilev1alpha1.EnvVar{
					{
						Name:  "AWS_REGION",
						Value: "us-west-2",
					},
				},
			},
			Backend: tfreconcilev1alpha1.BackendSpec{
				Type: "local",
			},
			ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
				{
					Name:    "aws",
					Version: ">= 5.83",
					Source:  "hashicorp/aws",
				},
			},
			Module: &tfreconcilev1alpha1.ModuleSpec{
				Name:    "test-module",
				Source:  "terraform-aws-modules/vpc/aws",
				Version: "5.21.0",
			},
		},
	}
	cf := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(obj).
		WithStatusSubresource(obj).
		Build()

	reconciler := &WorkspaceReconciler{
		Client:   cf,
		Scheme:   scheme,
		Tf:       runner.New(t.TempDir()),
		Recorder: record.NewFakeRecorder(10),
	}

	// When
	_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{
		NamespacedName: name,
	})
	require.NoError(t, err)

	// Then
	var ws tfreconcilev1alpha1.Workspace
	require.NoError(t, cf.Get(context.Background(), name, &ws))

	assert.Equal(t, ws.Name, "test-ws")
}
