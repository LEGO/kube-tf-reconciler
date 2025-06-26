package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/internal/testutils"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/support/kind"
)

func TestWorkspace(t *testing.T) {
	kindClusterName := "krec-cluster" //envconf.RandomName("my-cluster", 16)

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()

	k := kind.NewCluster(kindClusterName)
	_, err := k.Create(ctx)
	require.NoError(t, err)

	client, err := klient.New(k.KubernetesRestConfig())
	require.NoError(t, err)
	err = k.WaitForControlPlane(ctx, client)
	require.NoError(t, err)
	//defer k.Destroy(ctx)

	err = testutils.SetupCRDs(client, ctx, testutils.CRDFolder(), "*")
	assert.NoError(t, err)
	t.Cleanup(func() {
		err = testutils.TeardownCRDs(client, context.Background(), testutils.CRDFolder(), "*")
		assert.NoError(t, err)
	})

	t.Run("test 1", func(t *testing.T) {

		s := scheme.Scheme
		utilruntime.Must(clientgoscheme.AddToScheme(s))
		utilruntime.Must(tfreconcilev1alpha1.AddToScheme(s))

		slog.SetLogLoggerLevel(slog.LevelInfo)
		ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

		mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
			Scheme:                  s,
			HealthProbeBindAddress:  "0",
			LeaderElectionNamespace: "default",
			LeaderElection:          false,
			LeaderElectionID:        "krec-leader",
		})
		assert.NoError(t, err)

		reconciler := &WorkspaceReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Tf:       runner.New(t.TempDir()),
			Recorder: mgr.GetEventRecorderFor("krec"),
		}
		kl, err := klient.New(mgr.GetConfig())
		assert.NoError(t, err)

		err = reconciler.SetupWithManager(mgr)
		assert.NoError(t, err)

		go func() {
			err = mgr.Start(ctrl.SetupSignalHandler())
			assert.NoError(t, err)
		}()

		<-mgr.Elected()

		ws := newWorkspace()
		err = kl.Resources().Create(ctx, ws)
		assert.NoError(t, err)

		condition := conditions.New(kl.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration)
		err = wait.For(condition, wait.WithContext(ctx), wait.WithTimeout(10*time.Second))
		assert.NoError(t, err)

		expectedRender := `terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "1.0"
    }
  }
  backend "s3" {
    bucket = "my-bucket"
  }
}
provider "aws" {
}
module "my-module" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.19.0"
}
`
		assert.Equal(t, expectedRender, ws.Status.CurrentRender)
	})
}

func newWorkspace() *tfreconcilev1alpha1.Workspace {
	return &tfreconcilev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workspace",
			Namespace: "default",
		},
		Spec: tfreconcilev1alpha1.WorkspaceSpec{
			TerraformVersion: "1.11.2",
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
				Name:    "my-module",
				Source:  "terraform-aws-modules/vpc/aws",
				Version: "5.19.0",
			},
		},
	}
}
