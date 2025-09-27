package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/internal/testutils"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
)

func TestWorkspaceController(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	logf.SetLogger(logr.FromSlogHandler(handler))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{testutils.CRDFolder()},
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
	assert.NoError(t, err)
	k, err := klient.New(cfg)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: testEnv.Scheme,
	})
	assert.NoError(t, err)

	localModule := `resource "random_pet" "name" {
  length    = 2
  separator = "-"
}
`

	rootDir := t.TempDir()
	t.Logf("using root dir: %s", rootDir)
	err = (&WorkspaceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("krec"),

		Tf: runner.New(rootDir),
		Renderer: render.NewLocalModuleRenderer(rootDir, map[string][]byte{
			"my-module": []byte(localModule),
		}),
	}).SetupWithManager(mgr)

	go mgr.Start(ctx)

	t.Cleanup(func() {
		cancel()
		err = testEnv.Stop()
		assert.NoError(t, err)
	})

	t.Run("creating the custom resource for the Kind Workspace", func(t *testing.T) {
		resource := &tfreconcilev1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource-creation",
				Namespace: "default",
			},
			Spec: tfreconcilev1alpha1.WorkspaceSpec{
				Backend: tfreconcilev1alpha1.BackendSpec{
					Type: "local",
				},
				AutoApply:        true,
				TerraformVersion: "1.13.3",
				ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
					{
						Name:    "aws",
						Version: ">= 5.63.1",
						Source:  "hashicorp/aws",
					},
				},
				Module: &tfreconcilev1alpha1.ModuleSpec{
					Source: "my-module",
					Name:   "my-module",
				},
			},
		}
		assert.NoError(t, k8sClient.Create(ctx, resource))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, func(object k8s.Object) bool {
			d := object.(*tfreconcilev1alpha1.Workspace)
			return d.Status.ValidRender
		}), wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))
		assert.NoError(t, err)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 1,
			resources.WithFieldSelector(fmt.Sprintf("involvedObject.name=%s", resource.Name))), wait.WithContext(ctx))
		assert.NoError(t, err)
		var reasons []string
		for _, e := range events.Items {
			reasons = append(reasons, e.Reason)
		}

		plans := &tfreconcilev1alpha1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListMatchN(plans, 1, WithOwner(resource, testEnv.Scheme)),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))
		assert.NoError(t, err)
		assert.Len(t, plans.Items, 1)
		relevantPlan := plans.Items[0]
		assert.Equal(t, tfreconcilev1alpha1.PlanPhasePlanned, relevantPlan.Status.Phase)
		assert.NotEmpty(t, relevantPlan.Status.ApplyOutput)

		assert.Len(t, reasons, 3)
		assert.Contains(t, reasons, TFApplyEventReason)
		assert.Contains(t, reasons, TFPlanEventReason)
		assert.Contains(t, reasons, TFValidateEventReason)
	})

	t.Run("cleanup plans on deletion", func(t *testing.T) {
		resource := &tfreconcilev1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource-cleanup",
				Namespace: "default",
			},
			Spec: tfreconcilev1alpha1.WorkspaceSpec{
				Backend: tfreconcilev1alpha1.BackendSpec{
					Type: "local",
				},
				AutoApply:        false,
				TerraformVersion: "1.13.3",
				ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
					{
						Name:    "aws",
						Version: ">= 5.63.1",
						Source:  "hashicorp/aws",
					},
				},
				Module: &tfreconcilev1alpha1.ModuleSpec{
					Source: "my-module",
					Name:   "my-module",
				},
			},
		}
		assert.NoError(t, k8sClient.Create(ctx, resource))

		plans := &tfreconcilev1alpha1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListMatchN(plans, 1, WithOwner(resource, testEnv.Scheme)),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k8sClient.Delete(ctx, resource))

		err = wait.For(conditions.New(k.Resources()).ResourcesDeleted(plans), wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))
		assert.NoError(t, err)
	})
}

func WithOwner(owner client.Object, schema *runtime.Scheme) func(k8s.Object) bool {
	return func(object k8s.Object) bool {
		found, err := controllerutil.HasOwnerReference(object.GetOwnerReferences(), owner, schema)
		if err != nil {
			return false
		}

		return found
	}
}
