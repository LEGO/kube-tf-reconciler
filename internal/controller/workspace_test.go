package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tfv1alphav1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/internal/testutils"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/go-logr/logr"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute*10)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{testutils.CRDFolder()},
		BinaryAssetsDirectory: testutils.GetFirstFoundEnvTestBinaryDir(),
		ErrorIfCRDPathMissing: true,
		Scheme:                k8sscheme.Scheme,
	}

	modHost, shutdown := testutils.NewModuleHost()

	err := tfv1alphav1.AddToScheme(testEnv.Scheme)
	assert.NoError(t, err)

	err = coordinationv1.AddToScheme(testEnv.Scheme)
	assert.NoError(t, err)

	cfg, err := testEnv.Start()
	assert.NoError(t, err)
	assert.NotNil(t, cfg)
	k, err := klient.New(cfg)
	assert.NoError(t, err)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testEnv.Scheme,
		LeaderElection:         false,
		HealthProbeBindAddress: "0",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	assert.NoError(t, err)

	moduleStateServer := testutils.NewModuleStateServer(t)
	t.Cleanup(func() {
		moduleStateServer.Close()
	})
	modHost.AddFileToModule("my-module", "main.tf", moduleStateServer.GetModule("my-module"))

	rootDir := t.TempDir() // testutils.TestDataFolder() // Enable to better introspection into test data
	t.Logf("using root dir: %s", rootDir)
	err = (&WorkspaceReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("krec"),

		Tf:       runner.New(rootDir),
		Renderer: render.NewFileRender(rootDir),
	}).SetupWithManager(ctx, mgr)

	go mgr.Start(ctx)

	t.Cleanup(func() {
		cancel()
		shutdown()
		assert.NoError(t, testEnv.Stop())
		assert.NoError(t, os.RemoveAll(filepath.Join(rootDir, "workspaces")))
	})

	t.Run("creating the custom resource for the Kind Workspace", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-creation", modHost.ModuleSource("my-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.NotEmpty(t, ws.Status.LastPlanOutput)
		assert.NotEmpty(t, ws.Status.InitOutput)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 3, testutils.EventOwnedBy(ws.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)
		var reasons []string
		for _, e := range events.Items {
			reasons = append(reasons, e.Reason)
		}

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)), wait.WithContext(ctx))

		assert.NoError(t, err)
		require.Len(t, plans.Items, 1)
		relevantPlan := plans.Items[0]
		assert.Equal(t, tfv1alphav1.PlanPhaseApplied, relevantPlan.Status.Phase)
		assert.NotEmpty(t, relevantPlan.Status.PlanOutput)
		assert.NotEmpty(t, relevantPlan.Status.ApplyOutput)

		assert.Len(t, reasons, 3)
		assert.Contains(t, reasons, TFApplyEventReason)
		assert.Contains(t, reasons, TFPlanEventReason)
		assert.Contains(t, reasons, TFValidateEventReason)
	})

	t.Run("manual apply request", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-manual-apply", modHost.ModuleSource("my-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = false
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		ws.Annotations = map[string]string{
			tfv1alphav1.ManualApplyAnnotation: "true",
		}
		assert.NoError(t, k.Resources().Update(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			_, ok := object.GetAnnotations()[tfv1alphav1.ManualApplyAnnotation]
			return !ok
		}))
		assert.NoError(t, err)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 3, testutils.EventOwnedBy(ws.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)
		var reasons []string
		for _, e := range events.Items {
			reasons = append(reasons, e.Reason)
		}

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)), wait.WithContext(ctx))

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

	t.Run("manual apply request is invalidated after workspace change", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-manual-apply-invalidated", modHost.ModuleSource("my-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = false
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err := wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.Equal(t, int64(1), ws.Generation)

		ws.Spec.Module.Inputs = testutils.Json(map[string]interface{}{
			"pet_name_length": 3,
		})
		ws.Annotations = map[string]string{
			tfv1alphav1.ManualApplyAnnotation: "true",
		}
		assert.NoError(t, k.Resources().Update(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			w := object.(*tfv1alphav1.Workspace)
			_, manualRequested := w.Annotations[tfv1alphav1.ManualApplyAnnotation]
			return w.Generation == 2 &&
				w.Status.ObservedGeneration == 2 &&
				!manualRequested &&
				w.Status.CurrentPlan != nil &&
				w.Status.CurrentPlan.Name == fmt.Sprintf("%s-2", w.Name)
		}), wait.WithContext(ctx))
		assert.NoError(t, err)

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 2, plansForWs(ws)), wait.WithContext(ctx))
		assert.NoError(t, err)
		require.Len(t, plans.Items, 2)
		for _, plan := range plans.Items {
			if plan.Name == fmt.Sprintf("%s-2", ws.Name) {
				assert.Equal(t, tfv1alphav1.PlanPhasePlanned, plan.Status.Phase)
				assert.Empty(t, plan.Status.ApplyOutput)
			}
		}
	})

	t.Run("authenticate with generic token", func(t *testing.T) {
		t.Parallel()
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

		ws := newWs("test-resource-generic-token", modHost.ModuleSource("my-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = false
		ws.Spec.Authentication = &tfv1alphav1.AuthenticationSpec{
			Tokens: []tfv1alphav1.TokenAuthConfig{
				{
					SecretKeyRef: tfv1alphav1.SecretKeySelector{
						Name: "my-secret",
						Key:  "token",
					},
					FilePathEnv: "AWS_TOKEN_FILE",
				},
			},
			AWS: &tfv1alphav1.AWSAuthConfig{
				ServiceAccountName: "default",
				RoleARN:            "test-arn",
			},
		}

		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.FileExists(t, filepath.Join(rootDir, "workspaces", ws.Namespace, ws.Name, "aws_token_file-token"))
		assert.FileExists(t, filepath.Join(rootDir, "workspaces", ws.Namespace, ws.Name, "aws-token"))
	})

	t.Run("skip destroy on deletion", func(t *testing.T) {
		t.Parallel()
		modName := "skip-destroy"
		modHost.AddFileToModule(modName, "main.tf", moduleStateServer.GetModule(modName))
		ws := newWs("test-skip-destroy", modHost.ModuleSource(modName))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourSkip
		ws.Spec.AutoApply = true
		ws.Spec.Module.Name = modName

		assert.NoError(t, k.Resources().Create(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.Equal(t, testutils.ModuleEventApply, moduleStateServer.CurrentStatus(modName))

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k.Resources().Delete(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceDeleted(ws), wait.WithContext(ctx))
		assert.NoError(t, err)
		// Not destroyed because we skipped
		assert.Equal(t, testutils.ModuleEventApply, moduleStateServer.CurrentStatus(modName))
	})

	t.Run("auto destroy on deletion", func(t *testing.T) {
		t.Parallel()
		modName := "auto-destroy"
		modHost.AddFileToModule(modName, "main.tf", moduleStateServer.GetModule(modName))
		ws := newWs("test-auto-destroy", modHost.ModuleSource(modName))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		ws.Spec.Module.Name = modName

		assert.NoError(t, k.Resources().Create(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.Equal(t, testutils.ModuleEventApply, moduleStateServer.CurrentStatus(modName))

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k.Resources().Delete(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceDeleted(ws), wait.WithContext(ctx))
		assert.NoError(t, err)
		// Auto destroyed
		assert.Equal(t, testutils.ModuleEventDestroy, moduleStateServer.CurrentStatus(modName))
	})

	t.Run("await manual destroy on deletion", func(t *testing.T) {
		t.Parallel()
		modName := "manual-destroy"
		modHost.AddFileToModule(modName, "main.tf", moduleStateServer.GetModule(modName))
		ws := newWs("test-manual-destroy", modHost.ModuleSource(modName))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourManual
		ws.Spec.AutoApply = true
		ws.Spec.Module.Name = modName

		assert.NoError(t, k.Resources().Create(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)
		assert.Equal(t, testutils.ModuleEventApply, moduleStateServer.CurrentStatus(modName))

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k.Resources().Delete(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsTerraformMessage("Auto-destroy is disabled, awaiting manual destroy")), wait.WithContext(ctx))
		assert.NoError(t, err)
		assert.Equal(t, testutils.ModuleEventApply, moduleStateServer.CurrentStatus(modName))

		ws.Annotations = map[string]string{
			tfv1alphav1.ManualDestroyAnnotation: "true",
		}
		assert.NoError(t, k.Resources().Update(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceDeleted(ws), wait.WithContext(ctx))
		assert.NoError(t, err)
		assert.Equal(t, testutils.ModuleEventDestroy, moduleStateServer.CurrentStatus(modName))
	})

	t.Run("cleanup plans on deletion", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-cleanup", modHost.ModuleSource("my-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = false

		assert.NoError(t, k.Resources().Create(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 0, plansForWs(ws)), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.NoError(t, k.Resources().Delete(ctx, ws))
		err = wait.For(conditions.New(k.Resources()).ResourceDeleted(ws), wait.WithContext(ctx))
		assert.NoError(t, err)

		for _, p := range plans.Items {
			owned, err := controllerutil.HasOwnerReference(p.OwnerReferences, ws, mgr.GetScheme())
			assert.NoError(t, err)
			assert.True(t, owned)
		}
	})

	t.Run("cleanup plans on passed history limit", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-history-limit-1", modHost.ModuleSource("my-module"))
		ws.Spec.AutoApply = false
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
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

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 1, plansForWs(ws)), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.Equal(t, 2, int(ws.Generation))
		assert.Equal(t, fmt.Sprintf("%s-2", ws.Name), plans.Items[0].Name)
	})

	t.Run("error message persisted on terraform init failure", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-error-init-failure", "https://invalid-source-that-does-not-exist.com/module")
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			w := object.(*tfv1alphav1.Workspace)
			return w.Status.TerraformPhase == TFPhaseErrored &&
				w.Status.LastErrorMessage != "" &&
				w.Status.LastErrorTime != nil
		}), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Equal(t, TFPhaseErrored, ws.Status.TerraformPhase)
		assert.Contains(t, ws.Status.LastErrorMessage, "Failed to download module")
		assert.NotNil(t, ws.Status.LastErrorTime)
		assert.Equal(t, ws.Status.TerraformMessage, ws.Status.LastErrorMessage)
	})

	t.Run("error message persisted on terraform validation failure", func(t *testing.T) {
		t.Parallel()
		invalidModule := `resource "invalid_resource_type" "test" {
  invalid_attribute = "value"
}`
		modHost.AddFileToModule("invalid-module", "main.tf", invalidModule)

		ws := newWs("test-error-validation-failure", modHost.ModuleSource("invalid-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			w := object.(*tfv1alphav1.Workspace)
			return w.Status.TerraformPhase == TFPhaseErrored &&
				w.Status.LastErrorMessage != "" &&
				w.Status.LastErrorTime != nil
		}), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Equal(t, TFPhaseErrored, ws.Status.TerraformPhase)
		assert.NotEmpty(t, ws.Status.LastErrorMessage)
		assert.NotNil(t, ws.Status.LastErrorTime)
		assert.False(t, ws.Status.ValidRender)
	})

	t.Run("error message persisted on terraform plan failure", func(t *testing.T) {
		t.Parallel()
		planFailModule := `variable "required_var" {
  type    = string
  default = "not_a_number"
}

resource "random_pet" "name" {
  length = var.required_var
}`
		modHost.AddFileToModule("plan-fail-module", "main.tf", planFailModule)

		ws := newWs("test-error-plan-failure", modHost.ModuleSource("plan-fail-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			w := object.(*tfv1alphav1.Workspace)
			return w.Status.TerraformPhase == TFPhaseErrored &&
				w.Status.LastErrorMessage != "" &&
				w.Status.LastErrorTime != nil
		}), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Equal(t, TFPhaseErrored, ws.Status.TerraformPhase)
		assert.Contains(t, ws.Status.LastErrorMessage, "Failed to execute terraform plan")
		assert.NotNil(t, ws.Status.LastErrorTime)
	})

	t.Run("error message persisted on terraform apply failure", func(t *testing.T) {
		t.Parallel()
		// Use a module that will pass plan but fail during apply
		// This uses a null_resource with a local-exec provisioner that will fail
		applyFailModule := `resource "null_resource" "fail" {
  provisioner "local-exec" {
    command = "exit 1"
  }
}`
		modHost.AddFileToModule("apply-fail-module", "main.tf", applyFailModule)

		ws := newWs("test-error-apply-failure", modHost.ModuleSource("apply-fail-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		// Add null provider before creating the workspace
		ws.Spec.ProviderSpecs = append(ws.Spec.ProviderSpecs, tfv1alphav1.ProviderSpec{
			Name:    "null",
			Version: "3.2.1",
			Source:  "hashicorp/null",
		})
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			w := object.(*tfv1alphav1.Workspace)
			return w.Status.TerraformPhase == TFPhaseErrored &&
				w.Status.LastErrorMessage != "" &&
				w.Status.LastErrorTime != nil
		}), wait.WithContext(ctx))
		assert.NoError(t, err)

		assert.Equal(t, TFPhaseErrored, ws.Status.TerraformPhase)
		assert.Contains(t, ws.Status.LastErrorMessage, "Failed to apply terraform")
		assert.NotNil(t, ws.Status.LastErrorTime)
		assert.NotEmpty(t, ws.Status.LastApplyOutput)
	})

	t.Run("error message cleared on successful reconciliation", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-error-cleared", modHost.ModuleSource("my-module"))
		ws.Spec.Destroy = tfv1alphav1.DestroyBehaviourAuto
		ws.Spec.AutoApply = true
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, testutils.WsCurrentGeneration))
		assert.NoError(t, err)

		assert.Equal(t, TFPhaseCompleted, ws.Status.TerraformPhase)
		assert.NotContains(t, ws.Status.TerraformMessage, "Failed")
	})
}

func TestHandleManualRetry(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	newReconciler := func(objs ...client.Object) (*WorkspaceReconciler, client.Client) {
		builder := clientfake.NewClientBuilder().WithScheme(scheme)
		for _, o := range objs {
			builder = builder.WithStatusSubresource(o)
		}
		c := builder.WithObjects(objs...).Build()
		return &WorkspaceReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(32),
			leases:   &sync.Map{},
		}, c
	}

	backoffWs := func(name string) *tfv1alphav1.Workspace {
		ws := newWs(name, "some-source")
		ws.Status.Backoff = tfv1alphav1.BackoffStatus{
			NextRetryTime: &metav1.Time{Time: time.Now().Add(10 * time.Minute)},
			RetryCount:    3,
		}
		return ws
	}

	t.Run("no-op when annotation absent", func(t *testing.T) {
		ws := backoffWs("retry-noop")
		r, c := newReconciler(ws)

		_, err, ret := r.handleManualRetry(t.Context(), ws)
		require.NoError(t, err)
		assert.False(t, ret)

		var got tfv1alphav1.Workspace
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ws), &got))
		// backoff untouched
		assert.NotNil(t, got.Status.Backoff.NextRetryTime)
		assert.Equal(t, int32(3), got.Status.Backoff.RetryCount)
	})

	t.Run("clears backoff and annotation when requested", func(t *testing.T) {
		ws := backoffWs("retry-clears")
		ws.Annotations = map[string]string{
			tfv1alphav1.ManualRetryAnnotation: "true",
		}
		r, c := newReconciler(ws)

		// metav1.Now() truncates to seconds, so compare against a second-truncated baseline
		before := metav1.Now().Add(-time.Second)
		_, err, ret := r.handleManualRetry(t.Context(), ws)
		require.NoError(t, err)
		// ret is false so reconciliation continues after clearing backoff
		assert.False(t, ret)

		var got tfv1alphav1.Workspace
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ws), &got))

		assert.Nil(t, got.Status.Backoff.NextRetryTime)
		assert.Equal(t, int32(0), got.Status.Backoff.RetryCount)
		assert.False(t, got.Status.NextRefreshTimestamp.IsZero(), "next refresh should be set")
		assert.False(t, got.Status.NextRefreshTimestamp.Time.Before(before))

		_, ok := got.Annotations[tfv1alphav1.ManualRetryAnnotation]
		assert.False(t, ok, "manual retry annotation should be removed")
	})

	t.Run("returns error when workspace missing", func(t *testing.T) {
		ws := backoffWs("retry-missing")
		ws.Annotations = map[string]string{
			tfv1alphav1.ManualRetryAnnotation: "true",
		}
		// reconciler with no objects: Get inside RetryOnConflict fails
		r, _ := newReconciler()

		_, err, ret := r.handleManualRetry(t.Context(), ws)
		require.Error(t, err)
		assert.True(t, ret)
		assert.Contains(t, err.Error(), "failed to clear backoff during manual retry")
	})
}

func TestExecuteTerraformApplyUsesSavedPlan(t *testing.T) {
	t.Run("passes the saved plan to apply", func(t *testing.T) {
		tool := &recordingIaCTool{}
		r := &WorkspaceReconciler{}

		_, err := r.executeTerraformApply(t.Context(), tool, false, func(string, string) {})
		require.NoError(t, err)
		assert.Equal(t, 1, tool.applyCalls)
		assert.Equal(t, planOutputPath, tool.applyPlanPath)
	})

	t.Run("returns missing saved plan error without a fallback apply", func(t *testing.T) {
		tool := &recordingIaCTool{applyErr: os.ErrNotExist}
		r := &WorkspaceReconciler{}

		_, err := r.executeTerraformApply(t.Context(), tool, false, func(string, string) {})
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Equal(t, 1, tool.applyCalls)
		assert.Equal(t, planOutputPath, tool.applyPlanPath)
	})
}

func TestSavedPlanValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	ws := &tfv1alphav1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "default", UID: "workspace-uid", Generation: 1},
		Status: tfv1alphav1.WorkspaceStatus{
			CurrentRender: "rendered configuration",
			CurrentPlan:   &tfv1alphav1.PlanReference{Name: "workspace-1", Namespace: "default"},
		},
	}
	rootDir := t.TempDir()
	r := &WorkspaceReconciler{Tf: runner.New(rootDir), Scheme: scheme}
	require.NoError(t, r.Tf.SetupWorkspace(ws))
	planPath := filepath.Join(r.Tf.GetWorkspacePath(ws), planOutputPath)
	require.NoError(t, os.WriteFile(planPath, []byte("saved plan"), 0600))
	digest, err := r.savedPlanDigest(ws)
	require.NoError(t, err)

	plan := &tfv1alphav1.Plan{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "workspace-1",
			Namespace:   "default",
			Annotations: savedPlanAnnotations(&savedPlanIdentity{workspaceGeneration: 1, contentHash: "content-hash", digest: digest}),
		},
		Spec: tfv1alphav1.PlanSpec{
			WorkspaceRef: tfv1alphav1.WorkspaceReference{Name: ws.Name, Namespace: ws.Namespace},
			Render:       ws.Status.CurrentRender,
		},
		Status: tfv1alphav1.PlanStatus{Phase: tfv1alphav1.PlanPhasePlanned},
	}
	require.NoError(t, controllerutil.SetControllerReference(ws, plan, scheme))
	r.Client = clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, plan).Build()

	require.NoError(t, r.validateSavedPlan(t.Context(), ws, "content-hash", true))

	ws.Status.CurrentRender = "changed configuration"
	assert.ErrorAs(t, r.validateSavedPlan(t.Context(), ws, "content-hash", false), new(*savedPlanIdentityMismatchError))
	ws.Status.CurrentRender = plan.Spec.Render

	assert.ErrorAs(t, r.validateSavedPlan(t.Context(), ws, "changed-content-hash", false), new(*savedPlanIdentityMismatchError))

	require.NoError(t, os.WriteFile(planPath, []byte("modified saved plan"), 0600))
	assert.ErrorAs(t, r.validateSavedPlan(t.Context(), ws, "content-hash", true), new(*savedPlanIdentityMismatchError))

	require.NoError(t, os.Remove(planPath))
	assert.ErrorIs(t, r.validateSavedPlan(t.Context(), ws, "content-hash", true), os.ErrNotExist)

	plan.Status.Phase = tfv1alphav1.PlanPhaseApplied
	require.NoError(t, r.Client.Update(t.Context(), plan))
	assert.ErrorAs(t, r.validateSavedPlan(t.Context(), ws, "content-hash", false), new(*savedPlanIdentityMismatchError))
}

func TestCreatePlanRecordRefreshesSavedPlanIdentityOnlyWhenPlanned(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	ws := &tfv1alphav1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "default", UID: "workspace-uid", Generation: 1},
		Status:     tfv1alphav1.WorkspaceStatus{CurrentRender: "rendered configuration"},
	}
	r := &WorkspaceReconciler{
		Client: clientfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&tfv1alphav1.Plan{}).WithObjects(ws).Build(),
		Scheme: scheme,
	}

	first := savedPlanIdentity{workspaceGeneration: 1, contentHash: "first-content", digest: "first-digest"}
	plan, err := r.createPlanRecord(t.Context(), ws, true, "first plan", "", tfv1alphav1.PlanPhasePlanned, false, &first)
	require.NoError(t, err)

	second := savedPlanIdentity{workspaceGeneration: 1, contentHash: "second-content", digest: "second-digest"}
	_, err = r.createPlanRecord(t.Context(), ws, true, "second plan", "", tfv1alphav1.PlanPhasePlanned, false, &second)
	require.NoError(t, err)

	var got tfv1alphav1.Plan
	require.NoError(t, r.Client.Get(t.Context(), client.ObjectKeyFromObject(plan), &got))
	assert.Equal(t, "second-content", got.Annotations[planContentHashAnnotation])
	assert.Equal(t, "second-digest", got.Annotations[planDigestAnnotation])

	_, err = r.createPlanRecord(t.Context(), ws, true, "second plan", "apply output", tfv1alphav1.PlanPhaseApplied, false, nil)
	require.NoError(t, err)
	require.NoError(t, r.Client.Get(t.Context(), client.ObjectKeyFromObject(plan), &got))
	assert.Equal(t, "second-content", got.Annotations[planContentHashAnnotation])
	assert.Equal(t, "second-digest", got.Annotations[planDigestAnnotation])
}

func TestInvalidateManualApplyRequiresNewApproval(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	ws := &tfv1alphav1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "workspace",
			Namespace:   "default",
			Annotations: map[string]string{tfv1alphav1.ManualApplyAnnotation: "true"},
		},
		Status: tfv1alphav1.WorkspaceStatus{ObservedGeneration: 1},
	}
	r := &WorkspaceReconciler{
		Client: clientfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ws).WithObjects(ws).Build(),
	}

	require.NoError(t, r.invalidateManualApply(t.Context(), ws, "new-content-hash"))

	var got tfv1alphav1.Workspace
	require.NoError(t, r.Client.Get(t.Context(), client.ObjectKeyFromObject(ws), &got))
	assert.NotContains(t, got.Annotations, tfv1alphav1.ManualApplyAnnotation)
	assert.True(t, got.Status.NewPlanNeeded)
	assert.False(t, got.Status.NewApplyNeeded)
	assert.Equal(t, int64(1), got.Status.ObservedGeneration)
}

func TestHandleApplyValidatesSavedPlanArtifact(t *testing.T) {
	newSetup := func(t *testing.T, artifact []byte) (*WorkspaceReconciler, *tfv1alphav1.Workspace, *recordingIaCTool) {
		t.Helper()
		scheme := runtime.NewScheme()
		require.NoError(t, tfv1alphav1.AddToScheme(scheme))

		ws := &tfv1alphav1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "default", UID: "workspace-uid", Generation: 1},
			Spec:       tfv1alphav1.WorkspaceSpec{AutoApply: true},
			Status: tfv1alphav1.WorkspaceStatus{
				CurrentRender:      "rendered configuration",
				CurrentPlan:        &tfv1alphav1.PlanReference{Name: "workspace-1", Namespace: "default"},
				HasChanges:         true,
				NewApplyNeeded:     true,
				ObservedGeneration: 0,
			},
		}
		rootDir := t.TempDir()
		exec := runner.New(rootDir)
		require.NoError(t, exec.SetupWorkspace(ws))
		dependencyPath := filepath.Join(exec.GetWorkspacePath(ws), ".terraform", "dependency")
		require.NoError(t, os.MkdirAll(filepath.Dir(dependencyPath), 0755))
		require.NoError(t, os.WriteFile(dependencyPath, []byte("dependency"), 0600))
		contentHash, err := exec.CalculateChecksum(ws)
		require.NoError(t, err)

		var digest string
		if artifact != nil {
			require.NoError(t, os.WriteFile(filepath.Join(exec.GetWorkspacePath(ws), planOutputPath), artifact, 0600))
			hash := sha256.Sum256(artifact)
			digest = fmt.Sprintf("%x", hash[:])
		} else {
			digest = "missing-artifact-digest"
		}
		plan := &tfv1alphav1.Plan{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "workspace-1",
				Namespace:   "default",
				Annotations: savedPlanAnnotations(&savedPlanIdentity{workspaceGeneration: 1, contentHash: contentHash, digest: digest}),
			},
			Spec: tfv1alphav1.PlanSpec{
				WorkspaceRef: tfv1alphav1.WorkspaceReference{Name: ws.Name, Namespace: ws.Namespace},
				Render:       ws.Status.CurrentRender,
			},
			Status: tfv1alphav1.PlanStatus{Phase: tfv1alphav1.PlanPhasePlanned},
		}
		require.NoError(t, controllerutil.SetControllerReference(ws, plan, scheme))
		r := &WorkspaceReconciler{
			Client:   clientfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ws, plan).WithObjects(ws, plan).Build(),
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(16),
			Tf:       exec,
		}
		return r, ws, &recordingIaCTool{}
	}

	t.Run("applies a matching saved plan", func(t *testing.T) {
		r, ws, tool := newSetup(t, []byte("saved plan"))

		_, err, ret := r.handleApply(t.Context(), ws, tool)
		require.NoError(t, err)
		assert.False(t, ret)
		assert.Equal(t, 1, tool.applyCalls)
		assert.Equal(t, planOutputPath, tool.applyPlanPath)
	})

	t.Run("applies a matching manually approved saved plan", func(t *testing.T) {
		r, ws, tool := newSetup(t, []byte("saved plan"))
		ws.Spec.AutoApply = false
		ws.Annotations = map[string]string{tfv1alphav1.ManualApplyAnnotation: "true"}
		require.NoError(t, r.Client.Update(t.Context(), ws))

		_, err, ret := r.handleApply(t.Context(), ws, tool)
		require.NoError(t, err)
		assert.False(t, ret)
		assert.Equal(t, 1, tool.applyCalls)
	})

	t.Run("rejects a missing saved plan before apply", func(t *testing.T) {
		r, ws, tool := newSetup(t, nil)

		_, err, ret := r.handleApply(t.Context(), ws, tool)
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.True(t, ret)
		assert.Zero(t, tool.applyCalls)

		var got tfv1alphav1.Workspace
		require.NoError(t, r.Client.Get(t.Context(), client.ObjectKeyFromObject(ws), &got))
		assert.Equal(t, int64(0), got.Status.ObservedGeneration)
	})

	t.Run("requeues when the API server has a newer workspace generation", func(t *testing.T) {
		r, ws, tool := newSetup(t, []byte("saved plan"))
		latest := ws.DeepCopy()
		latest.Generation++
		r.APIReader = clientfake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(latest).Build()

		result, err, ret := r.handleApply(t.Context(), ws, tool)
		require.NoError(t, err)
		assert.True(t, ret)
		assert.True(t, result.Requeue)
		assert.Zero(t, tool.applyCalls)
	})

	t.Run("does not apply when manual approval was removed", func(t *testing.T) {
		r, ws, tool := newSetup(t, []byte("saved plan"))
		ws.Spec.AutoApply = false
		ws.Annotations = map[string]string{tfv1alphav1.ManualApplyAnnotation: "true"}
		require.NoError(t, r.Client.Update(t.Context(), ws))

		latest := ws.DeepCopy()
		delete(latest.Annotations, tfv1alphav1.ManualApplyAnnotation)
		r.APIReader = clientfake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(latest).Build()

		_, err, ret := r.handleApply(t.Context(), ws, tool)
		require.NoError(t, err)
		assert.False(t, ret)
		assert.Zero(t, tool.applyCalls)
	})
}

func TestHandlePlanRejectsStaleRenderedGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	ws := &tfv1alphav1.Workspace{
		// The status update after rendering has refreshed this pointer to generation 2,
		// while the files on disk were rendered from generation 1.
		ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "default", Generation: 2},
		Status:     tfv1alphav1.WorkspaceStatus{NewPlanNeeded: true},
	}
	latest := ws.DeepCopy()
	r := &WorkspaceReconciler{
		Client:    clientfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ws).WithObjects(ws).Build(),
		APIReader: clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(latest).Build(),
		Scheme:    scheme,
		Recorder:  record.NewFakeRecorder(16),
	}
	tool := &recordingIaCTool{}

	result, err, ret := r.handlePlan(t.Context(), ws, tool, 1)
	require.NoError(t, err)
	assert.True(t, ret)
	assert.True(t, result.Requeue)
	assert.Zero(t, tool.planCalls)
}

type recordingIaCTool struct {
	planCalls     int
	applyCalls    int
	applyPlanPath string
	applyErr      error
	stdout        io.Writer
	stderr        io.Writer
}

func (t *recordingIaCTool) Name() string { return "recording" }

func (t *recordingIaCTool) Init(context.Context, ...runner.InitOption) error { return nil }

func (t *recordingIaCTool) Validate(context.Context) (*tfjson.ValidateOutput, error) { return nil, nil }

func (t *recordingIaCTool) Plan(context.Context, ...runner.PlanOption) (bool, error) {
	t.planCalls++
	return false, nil
}

func (t *recordingIaCTool) ShowPlanFileRaw(context.Context, string) (string, error) { return "", nil }

func (t *recordingIaCTool) Apply(_ context.Context, planPath string) error {
	t.applyCalls++
	t.applyPlanPath = planPath
	return t.applyErr
}

func (t *recordingIaCTool) Destroy(context.Context) error { return nil }

func (t *recordingIaCTool) SetEnv(map[string]string) error { return nil }

func (t *recordingIaCTool) SetStdout(w io.Writer) { t.stdout = w }

func (t *recordingIaCTool) SetStderr(w io.Writer) { t.stderr = w }

func plansForWs(ws *tfv1alphav1.Workspace) resources.ListOption {
	return resources.WithLabelSelector(fmt.Sprintf("%s=%s", tfv1alphav1.WorkspacePlanLabel, ws.Name))
}

func newWs(name, moduleSource string) *tfv1alphav1.Workspace {
	return &tfv1alphav1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: tfv1alphav1.WorkspaceSpec{
			Backend: tfv1alphav1.BackendSpec{
				Type: "local",
			},
			AutoApply:      true,
			PreventDestroy: true,
			ToolVersion:    "1.13.3",
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
				Source: moduleSource,
				Name:   "my-module",
			},
		},
	}
}

func TestLeaseReconciliationBehaviour(t *testing.T) {
	modHost, shutdown := testutils.NewModuleHost()
	defer shutdown()

	localModule := `variable "pet_name_length" {
  default = 2
  type    = number
}

resource "random_pet" "name" {
  length    = var.pet_name_length
  separator = "-"
}
`
	modHost.AddFileToModule("my-module", "main.tf", localModule)

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))

	freeWs := newWs("test-free", modHost.ModuleSource("my-module"))
	lockedWs := newWs("test-locked", modHost.ModuleSource("my-module"))
	expiredWs := newWs("test-expired", modHost.ModuleSource("my-module"))
	now := metav1.NewMicroTime(time.Now())
	past := metav1.NewMicroTime(now.Add(-5 * time.Minute))
	otherLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName(lockedWs),
			Namespace: lockedWs.Namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("someone-else"),
			LeaseDurationSeconds: ptr.To[int32](300),
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	expiredLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName(expiredWs),
			Namespace: lockedWs.Namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("someone-else"),
			LeaseDurationSeconds: ptr.To[int32](300),
			AcquireTime:          &past,
			RenewTime:            &past,
		},
	}
	pp := tfv1alphav1.Plan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plan-for-expired-ws",
			Namespace: expiredWs.Namespace,
		},
	}
	client := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(freeWs, lockedWs, expiredWs, &pp).
		WithObjects(freeWs, lockedWs, expiredWs, otherLease, expiredLease).
		Build()

	rootDir := t.TempDir()
	reconciler := &WorkspaceReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(32),

		Tf:       runner.New(rootDir),
		Renderer: render.NewFileRender(rootDir),
		leases:   &sync.Map{},
	}

	t.Run("acquisition", func(t *testing.T) {
		t.Run("can acquire lease for free workspace", func(t *testing.T) {
			t.Cleanup(func() {
				reconciler.leases = &sync.Map{}
				assert.NoError(t, client.Delete(t.Context(), &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{
						Name:      leaseName(freeWs),
						Namespace: freeWs.Namespace,
					},
				}))
			})
			_, err, ret := reconciler.acquireLease(t.Context(), freeWs)
			assert.False(t, ret)
			assert.NoError(t, err)

			_, exists := reconciler.leases.Load(leaseName(freeWs))
			assert.True(t, exists)

			var lease coordinationv1.Lease
			err = client.Get(context.TODO(), types.NamespacedName{
				Name:      leaseName(freeWs),
				Namespace: freeWs.Namespace,
			}, &lease)
			assert.NoError(t, err)
		})

		t.Run("can not acquire lease for locked workspace", func(t *testing.T) {
			_, err, ret := reconciler.acquireLease(t.Context(), lockedWs)
			assert.True(t, ret)
			assert.NoError(t, err)
		})

		t.Run("can takeover expired lease", func(t *testing.T) {
			t.Cleanup(func() {
				reconciler.leases = &sync.Map{}
				assert.NoError(t, client.Delete(t.Context(), &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{
						Name:      leaseName(expiredWs),
						Namespace: expiredWs.Namespace,
					},
				}))
			})

			_, err, ret := reconciler.acquireLease(t.Context(), expiredWs)
			assert.False(t, ret)
			assert.NoError(t, err)

			_, exists := reconciler.leases.Load(leaseName(expiredWs))
			assert.True(t, exists)

			var lease coordinationv1.Lease
			err = client.Get(context.TODO(), types.NamespacedName{
				Name:      leaseName(expiredWs),
				Namespace: expiredWs.Namespace,
			}, &lease)
			assert.NoError(t, err)
		})
	})

	t.Run("release", func(t *testing.T) {
		t.Run("when releasing non-leased workspace, returns true but no error", func(t *testing.T) {
			_, err, ret := reconciler.releaseLease(t.Context(), freeWs)
			assert.NoError(t, err)
			assert.True(t, ret)
		})
		t.Run("can release lease for locked workspace", func(t *testing.T) {
			_, err, ret := reconciler.acquireLease(t.Context(), freeWs)
			assert.NoError(t, err)
			assert.False(t, ret)

			var lease coordinationv1.Lease
			err = client.Get(context.TODO(), types.NamespacedName{
				Name:      leaseName(freeWs),
				Namespace: freeWs.Namespace,
			}, &lease)
			assert.NoError(t, err)

			_, err, ret = reconciler.releaseLease(t.Context(), freeWs)
			assert.NoError(t, err)
			assert.False(t, ret)

			err = client.Get(context.TODO(), types.NamespacedName{
				Name:      leaseName(freeWs),
				Namespace: freeWs.Namespace,
			}, &lease)
			assert.True(t, apierrors.IsNotFound(err))
		})
		t.Run("will not release lease for workspace locked by others", func(t *testing.T) {
			_, err, ret := reconciler.releaseLease(t.Context(), freeWs)
			assert.NoError(t, err)
			assert.True(t, ret)

			var lease coordinationv1.Lease
			err = client.Get(context.TODO(), types.NamespacedName{
				Name:      leaseName(lockedWs),
				Namespace: lockedWs.Namespace,
			}, &lease)
			assert.NoError(t, err)
		})
	})

	t.Run("can acquire and free lease for free workspace", func(t *testing.T) {
		_, err := reconciler.Reconcile(context.TODO(), reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: freeWs.Namespace,
				Name:      freeWs.Name,
			},
		})

		require.NoError(t, err)
		var lease coordinationv1.Lease
		err = client.Get(context.TODO(), types.NamespacedName{
			Name:      leaseName(freeWs),
			Namespace: freeWs.Namespace,
		}, &lease)
		assert.True(t, apierrors.IsNotFound(err))
		var plan tfv1alphav1.Plan
		err = client.Get(context.TODO(), types.NamespacedName{
			Name:      fmt.Sprintf("%s-%d", freeWs.Name, freeWs.Generation),
			Namespace: freeWs.Namespace,
		}, &plan)
		assert.NoError(t, err)
	})
}
