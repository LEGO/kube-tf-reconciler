package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/e2e-framework/klient"
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
	defer cancel()

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

	rootDir := testutils.TestDataFolder()
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
		ws := newWs("test-resource-creation")
		ws.Spec.PreventDestroy = false
		ws.Spec.AutoApply = true
		assert.NoError(t, k8sClient.Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.NotEmpty(t, ws.Status.LastPlanOutput)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 3, testutils.EventOwnedBy(ws.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)
		var reasons []string
		for _, e := range events.Items {
			reasons = append(reasons, e.Reason)
		}

		plans := &tfreconcilev1alpha1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))

		assert.NoError(t, err)
		assert.Len(t, plans.Items, 1)
		relevantPlan := plans.Items[0]
		assert.Equal(t, tfreconcilev1alpha1.PlanPhaseApplied, relevantPlan.Status.Phase)
		assert.NotEmpty(t, relevantPlan.Status.ApplyOutput)

		assert.Len(t, reasons, 3)
		assert.Contains(t, reasons, TFApplyEventReason)
		assert.Contains(t, reasons, TFPlanEventReason)
		assert.Contains(t, reasons, TFValidateEventReason)
	})

	t.Run("authenticate with generic token", func(t *testing.T) {
		err = k.Resources().Create(ctx, &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-secret",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"token": []byte("token content string blip blop"),
			},
		})
		assert.NoError(t, err)

		err = k.Resources().Create(ctx, &v1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default",
				Namespace: "default",
			},
		})
		assert.NoError(t, err)

		ws := newWs("test-resource-generic-token")
		ws.Spec.PreventDestroy = false
		ws.Spec.AutoApply = false
		ws.Spec.Authentication = &tfreconcilev1alpha1.AuthenticationSpec{
			Tokens: []tfreconcilev1alpha1.TokenAuthConfig{
				{
					SecretKeyRef: tfreconcilev1alpha1.SecretKeySelector{
						Name: "my-secret",
						Key:  "token",
					},
					FilePathEnv: "AWS_TOKEN_FILE",
				},
			},
			AWS: &tfreconcilev1alpha1.AWSAuthConfig{
				ServiceAccountName: "default",
				RoleARN:            "test-arn",
			},
		}

		assert.NoError(t, k8sClient.Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.FileExists(t, filepath.Join(rootDir, "workspaces", ws.Namespace, ws.Name, "aws_token_file-token"))
		assert.FileExists(t, filepath.Join(rootDir, "workspaces", ws.Namespace, ws.Name, "aws-token"))
	})

	t.Run("cleanup plans on deletion", func(t *testing.T) {
		ws := newWs("test-resource-cleanup")
		ws.Spec.PreventDestroy = false
		ws.Spec.AutoApply = false
		assert.NoError(t, k.Resources().Create(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		plans := &tfreconcilev1alpha1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k.Resources().Delete(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceDeleted(ws),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))
		assert.NoError(t, err)

		emptyPlans := &tfreconcilev1alpha1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(emptyPlans, 0, plansForWs(ws)),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))
		assert.NoError(t, err)
	})

	t.Run("cleanup plans on passed history limit", func(t *testing.T) {
		ws := newWs("test-resource-history-limit-1")
		ws.Spec.AutoApply = false
		ws.Spec.PreventDestroy = false
		ws.Spec.PlanHistoryLimit = 1

		assert.NoError(t, k.Resources().Create(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		ws.Spec.Module.Inputs = testutils.Json(map[string]interface{}{
			"pet_name_length": 3,
		})
		assert.NoError(t, k.Resources().Update(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		plans := &tfreconcilev1alpha1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 1, plansForWs(ws)),
			wait.WithTimeout(time.Minute*1), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.Equal(t, 2, int(ws.Generation))
		assert.Equal(t, fmt.Sprintf("%s-2", ws.Name), plans.Items[0].Name)
	})
}

func plansForWs(resource *tfreconcilev1alpha1.Workspace) resources.ListOption {
	return resources.WithLabelSelector(fmt.Sprintf("%s=%s", workspacePlanLabel, resource.Name))
}

func newWs(name string) *tfreconcilev1alpha1.Workspace {
	return &tfreconcilev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: tfreconcilev1alpha1.WorkspaceSpec{
			Backend: tfreconcilev1alpha1.BackendSpec{
				Type: "local",
			},
			AutoApply:        true,
			PreventDestroy:   true,
			TerraformVersion: "1.13.3",
			ProviderSpecs: []tfreconcilev1alpha1.ProviderSpec{
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
			Module: &tfreconcilev1alpha1.ModuleSpec{
				Source: "my-module",
				Name:   "my-module",
			},
		},
	}
}
