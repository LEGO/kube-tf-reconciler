package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	tfv1alphav1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/internal/testutils"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
)

func init() {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	logf.SetLogger(logr.FromSlogHandler(slog.Default().Handler()).V(5))
}

func TestWorkspaceController(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{testutils.CRDFolder()},
		BinaryAssetsDirectory: testutils.GetFirstFoundEnvTestBinaryDir(),
		ErrorIfCRDPathMissing: true,
		Scheme:                k8sscheme.Scheme,
	}

	err := tfv1alphav1.AddToScheme(testEnv.Scheme)
	assert.NoError(t, err)

	cfg, err := testEnv.Start()
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	k, err := klient.New(cfg)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testEnv.Scheme,
		LeaderElection:         false,
		HealthProbeBindAddress: "0",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	assert.NoError(t, err)

	localModule := `variable "pet_name_length" {
  default = 2
  type    = number
}

resource "random_pet" "name" {
  length    = var.pet_name_length
  separator = "-"
}
`

	rootDir := t.TempDir() // testutils.TestDataFolder() // Enable to better introspection into test data
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
		err = os.RemoveAll(filepath.Join(rootDir, "workspaces"))
		assert.NoError(t, err)
	})

	t.Run("creating the custom resource for the Kind Workspace", func(t *testing.T) {
		t.Parallel()
		ctx = t.Context()

		resource := &tfv1alphav1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource-creation",
				Namespace: "default",
			},
			Spec: tfv1alphav1.WorkspaceSpec{
				Backend: tfv1alphav1.BackendSpec{
					Type: "local",
				},
				AutoApply:        true,
				PreventDestroy:   false,
				TerraformVersion: "1.13.3",
				ProviderSpecs: []tfv1alphav1.ProviderSpec{
					{
						Name:    "aws",
						Version: ">= 5.63.1",
						Source:  "hashicorp/aws",
					},
					{
						Name:    "random",
						Version: "3.7.2",
						Source:  "hashicorp/random",
					},
				},
				Module: &tfv1alphav1.ModuleSpec{
					Source: "my-module",
					Name:   "my-module",
				},
			},
		}
		assert.NoError(t, k.Resources().Create(ctx, resource))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.NotEmpty(t, resource.Status.LastPlanOutput)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 3, testutils.EventOwnedBy(resource.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)
		var reasons []string
		for _, e := range events.Items {
			reasons = append(reasons, e.Reason)
		}

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(resource)),
			wait.WithTimeout(time.Minute*2), wait.WithContext(ctx))

		assert.NoError(t, err)
		assert.Len(t, plans.Items, 1)
		relevantPlan := plans.Items[0]
		assert.Equal(t, tfv1alphav1.PlanPhaseApplied, relevantPlan.Status.Phase)
		assert.NotEmpty(t, relevantPlan.Status.ApplyOutput)

		assert.Len(t, reasons, 3)
		assert.Contains(t, reasons, TFApplyEventReason)
		assert.Contains(t, reasons, TFPlanEventReason)
		assert.Contains(t, reasons, TFValidateEventReason)
	})

	t.Run("manual apply request", func(t *testing.T) {
		t.Parallel()
		ctx = t.Context()

		resource := &tfv1alphav1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource-manual-apply",
				Namespace: "default",
			},
			Spec: tfv1alphav1.WorkspaceSpec{
				Backend: tfv1alphav1.BackendSpec{
					Type: "local",
				},
				AutoApply:        false,
				PreventDestroy:   false,
				TerraformVersion: "1.13.3",
				ProviderSpecs: []tfv1alphav1.ProviderSpec{
					{
						Name:    "aws",
						Version: ">= 5.63.1",
						Source:  "hashicorp/aws",
					},
					{
						Name:    "random",
						Version: "3.7.2",
						Source:  "hashicorp/random",
					},
				},
				Module: &tfv1alphav1.ModuleSpec{
					Source: "my-module",
					Name:   "my-module",
				},
			},
		}
		assert.NoError(t, k.Resources().Create(ctx, resource))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		resource.Annotations = map[string]string{
			tfv1alphav1.ManualApplyAnnotation: "true",
		}
		assert.NoError(t, k.Resources().Update(ctx, resource))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, func(object k8s.Object) bool {
			_, ok := object.GetAnnotations()[tfv1alphav1.ManualApplyAnnotation]
			return !ok
		}))
		assert.NoError(t, err)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 3, testutils.EventOwnedBy(resource.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)
		var reasons []string
		for _, e := range events.Items {
			reasons = append(reasons, e.Reason)
		}

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(resource)),
			wait.WithTimeout(time.Minute*2), wait.WithContext(ctx))

		assert.NoError(t, err)
		assert.Len(t, plans.Items, 1)
		relevantPlan := plans.Items[0]
		assert.Equal(t, tfv1alphav1.PlanPhaseApplied, relevantPlan.Status.Phase)
		assert.NotEmpty(t, relevantPlan.Status.ApplyOutput)

		assert.Len(t, reasons, 4)
		assert.Contains(t, reasons, TFApplyEventReason)
		assert.Contains(t, reasons, TFPlanEventReason)
		assert.Contains(t, reasons, TFValidateEventReason)
	})

	t.Run("cleanup plans on deletion", func(t *testing.T) {
		t.Parallel()
		ctx = t.Context()

		resource := &tfv1alphav1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource-cleanup",
				Namespace: "default",
			},
			Spec: tfv1alphav1.WorkspaceSpec{
				Backend: tfv1alphav1.BackendSpec{
					Type: "local",
				},
				AutoApply:        false,
				PreventDestroy:   false,
				TerraformVersion: "1.13.3",
				ProviderSpecs: []tfv1alphav1.ProviderSpec{
					{
						Name:    "aws",
						Version: ">= 5.63.1",
						Source:  "hashicorp/aws",
					},
				},
				Module: &tfv1alphav1.ModuleSpec{
					Source: "my-module",
					Name:   "my-module",
				},
			},
		}
		assert.NoError(t, k.Resources().Create(ctx, resource))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(resource)),
			wait.WithTimeout(time.Minute*2), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k.Resources().Delete(ctx, resource))
		err = wait.For(conditions.New(k.Resources()).ResourceDeleted(resource),
			wait.WithTimeout(time.Minute*2), wait.WithContext(ctx))
		assert.NoError(t, err)

		emptyPlans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(emptyPlans, 0, plansForWs(resource)),
			wait.WithTimeout(time.Minute*2), wait.WithContext(ctx))
		assert.NoError(t, err)
	})

	t.Run("cleanup plans on passed history limit", func(t *testing.T) {
		t.Parallel()
		ctx = t.Context()

		resource := &tfv1alphav1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-resource-history-limit-1",
				Namespace: "default",
			},
			Spec: tfv1alphav1.WorkspaceSpec{
				Backend: tfv1alphav1.BackendSpec{
					Type: "local",
				},
				AutoApply:        false,
				PreventDestroy:   false,
				PlanHistoryLimit: 1,
				TerraformVersion: "1.13.3",
				ProviderSpecs: []tfv1alphav1.ProviderSpec{
					{
						Name:    "aws",
						Version: ">= 5.63.1",
						Source:  "hashicorp/aws",
					},
				},
				Module: &tfv1alphav1.ModuleSpec{
					Source: "my-module",
					Name:   "my-module",
				},
			},
		}
		assert.NoError(t, k.Resources().Create(ctx, resource))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		resource.Spec.Module.Inputs = testutils.Json(map[string]interface{}{
			"pet_name_length": 3,
		})
		assert.NoError(t, k.Resources().Update(ctx, resource))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(resource, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 1, plansForWs(resource)),
			wait.WithTimeout(time.Minute*2), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.Equal(t, 2, int(resource.Generation))
		assert.Equal(t, fmt.Sprintf("%s-2", resource.Name), plans.Items[0].Name)
	})
}

func plansForWs(resource *tfv1alphav1.Workspace) resources.ListOption {
	return resources.WithLabelSelector(fmt.Sprintf("%s=%s", tfv1alphav1.WorkspacePlanLabel, resource.Name))
}
