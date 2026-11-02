package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("krec"),

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

func TestMissingSavedPlanIsReplannedBeforeManualApply(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	ws := newWs("missing-plan", "some-source")
	ws.Generation = 1
	ws.Spec.AutoApply = false
	ws.Annotations = map[string]string{tfv1alphav1.ManualApplyAnnotation: "true"}
	ws.Status = tfv1alphav1.WorkspaceStatus{
		CurrentPlan:    &tfv1alphav1.PlanReference{Name: "missing-plan-1", Namespace: ws.Namespace},
		HasChanges:     true,
		NewPlanNeeded:  false,
		NewApplyNeeded: false,
	}

	plan := &tfv1alphav1.Plan{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-plan-1", Namespace: ws.Namespace},
		Spec:       tfv1alphav1.PlanSpec{WorkspaceRef: tfv1alphav1.WorkspaceReference{Name: ws.Name, Namespace: ws.Namespace}},
	}
	rootDir := t.TempDir()
	exec := runner.New(rootDir)
	require.NoError(t, exec.SetupWorkspace(ws))
	require.NoError(t, os.Mkdir(filepath.Join(exec.GetWorkspacePath(ws), ".terraform"), 0755))
	planPath := filepath.Join(exec.GetWorkspacePath(ws), planOutputPath)
	require.NoError(t, os.WriteFile(planPath, []byte("old plan"), 0600))
	require.NoError(t, os.Remove(planPath))

	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(ws, plan).
		WithObjects(ws, plan).
		Build()
	tool := &replanningIaCTool{workspacePath: exec.GetWorkspacePath(ws), changed: true}
	r := &WorkspaceReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(32),
		Tf:       exec,
	}

	result, err, ret := r.handleRefreshDependencies(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.False(t, ret)
	assert.False(t, result.Requeue)
	assert.False(t, ws.Status.NewPlanNeeded)
	assert.True(t, ws.Status.NewApplyNeeded)
	assert.Zero(t, ws.Status.Backoff.RetryCount)

	result, err, ret = r.handlePlan(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.False(t, ret)
	assert.False(t, result.Requeue)
	assert.Zero(t, tool.planCalls)

	result, err, ret = r.handleApply(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.True(t, ret)
	assert.True(t, result.Requeue)
	assert.Equal(t, 0, tool.applyCalls)
	assert.True(t, ws.Status.NewPlanNeeded)
	assert.False(t, ws.Status.NewApplyNeeded)
	assert.Zero(t, ws.Status.Backoff.RetryCount)

	result, err, ret = r.handleRefreshDependencies(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.False(t, ret)
	assert.False(t, result.Requeue)
	assert.True(t, ws.Status.NewPlanNeeded)

	result, err, ret = r.handlePlan(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.False(t, ret)
	assert.False(t, result.Requeue)
	assert.Equal(t, 1, tool.planCalls)

	require.NoError(t, os.Remove(planPath))
	result, err, ret = r.handleApply(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.True(t, ret)
	assert.True(t, result.Requeue)
	assert.Equal(t, 0, tool.applyCalls)

	result, err, ret = r.handlePlan(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.False(t, ret)
	assert.False(t, result.Requeue)
	assert.Equal(t, 2, tool.planCalls)

	result, err, ret = r.handleApply(t.Context(), ws, tool)
	require.NoError(t, err)
	assert.False(t, ret)
	assert.False(t, result.Requeue)
	assert.Equal(t, 1, tool.applyCalls)
	assert.Equal(t, planOutputPath, tool.applyPlanPath)
	assert.FileExists(t, planPath)

	var got tfv1alphav1.Workspace
	require.NoError(t, fakeClient.Get(t.Context(), client.ObjectKeyFromObject(ws), &got))
	assert.Equal(t, "true", got.Annotations[tfv1alphav1.ManualApplyAnnotation])
	assert.Zero(t, got.Status.Backoff.RetryCount)
	assert.Equal(t, TFPhaseCompleted, got.Status.TerraformPhase)
}

type replanningIaCTool struct {
	workspacePath string
	changed       bool
	planCalls     int
	applyCalls    int
	applyPlanPath string
}

func (t *replanningIaCTool) Name() string                                     { return "terraform" }
func (t *replanningIaCTool) Init(context.Context, ...runner.InitOption) error { return nil }
func (t *replanningIaCTool) Validate(context.Context) (*tfjson.ValidateOutput, error) {
	return &tfjson.ValidateOutput{Valid: true}, nil
}
func (t *replanningIaCTool) Plan(_ context.Context, opts ...runner.PlanOption) (bool, error) {
	t.planCalls++
	options := runner.PlanOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	return t.changed, os.WriteFile(filepath.Join(t.workspacePath, options.Out), []byte("new plan"), 0600)
}
func (t *replanningIaCTool) ShowPlanFileRaw(context.Context, string) (string, error) {
	return "new plan", nil
}
func (t *replanningIaCTool) Apply(_ context.Context, planPath string) error {
	t.applyCalls++
	t.applyPlanPath = planPath
	_, err := os.Stat(filepath.Join(t.workspacePath, planPath))
	return err
}
func (t *replanningIaCTool) Destroy(context.Context) error  { return nil }
func (t *replanningIaCTool) SetEnv(map[string]string) error { return nil }
func (t *replanningIaCTool) SetStdout(io.Writer)            {}
func (t *replanningIaCTool) SetStderr(io.Writer)            {}

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

func pinnedGitModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
resource "random_pet" "name" {
  length = 2
}
`), 0644))

	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "module")
	sha := git("rev-parse", "HEAD")

	return fmt.Sprintf("git::file://%s?ref=%s", dir, sha)
}

func TestInitSkipDecision(t *testing.T) {
	const pinned = "git::https://github.com/LEGO/repo.git?ref=605beb1ed65a00b79edbcb74ff851d92399f598d"
	modulesJSON := `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"my-module","Source":"` + pinned + `","Dir":".terraform/modules/my-module"}]}`
	envs := map[string]string{"FOO": "bar"}

	setup := func(t *testing.T) (*WorkspaceReconciler, *tfv1alphav1.Workspace) {
		r := &WorkspaceReconciler{Tf: runner.New(t.TempDir())}
		ws := newWs("skip", pinned)
		ws.Generation = 2
		ws.Status.ObservedGeneration = 2
		ws.Status.TerraformPhase = TFPhaseCompleted
		ws.Status.ValidRender = true

		wsPath := r.Tf.GetWorkspacePath(ws)
		require.NoError(t, os.MkdirAll(filepath.Join(wsPath, ".terraform", "modules", "my-module"), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(wsPath, "main.tf"), []byte("module {}"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(wsPath, ".terraform", "modules", "modules.json"), []byte(modulesJSON), 0644))

		fp, err := r.Tf.InitFingerprint(ws, envs)
		require.NoError(t, err)
		require.NoError(t, r.Tf.WriteInitMarker(ws, fp))
		ws.Status.InitFingerprint = fp
		return r, ws
	}

	t.Run("skips when nothing changed", func(t *testing.T) {
		r, ws := setup(t)
		skip, reason := r.initSkipDecision(ws, envs, false)
		assert.True(t, skip, reason)
	})

	tests := []struct {
		name   string
		mutate func(t *testing.T, r *WorkspaceReconciler, ws *tfv1alphav1.Workspace)
		envs   map[string]string
		force  bool
		reason string
	}{
		{name: "manual retry", reason: "manual retry", force: true},
		{name: "deleting", reason: "deleted", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.DeletionTimestamp = ptr.To(metav1.Now())
		}},
		{name: "spec changed", reason: "spec changed", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) { ws.Generation = 3 }},
		{name: "previous run errored", reason: "did not complete", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Status.TerraformPhase = TFPhaseErrored
		}},
		{name: "previous run interrupted", reason: "did not complete", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Status.TerraformPhase = TFPhasePlanning
		}},
		{name: "backing off", reason: "did not complete", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Status.Backoff.RetryCount = 1
		}},
		{name: "invalid render", reason: "did not complete", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Status.ValidRender = false
		}},
		{name: "floating root module", reason: "not pinned", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Spec.Module.Source = "git::https://github.com/LEGO/repo.git?ref=main"
		}},
		{name: "no marker", reason: "no previous successful init", mutate: func(t *testing.T, r *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			require.NoError(t, r.Tf.RemoveInitMarker(ws))
		}},
		{name: "operator restarted", reason: "operator start", mutate: func(_ *testing.T, r *WorkspaceReconciler, _ *tfv1alphav1.Workspace) {
			r.Tf.BootID = "another-process"
		}},
		{name: "force init interval elapsed", reason: "older than", mutate: func(_ *testing.T, r *WorkspaceReconciler, _ *tfv1alphav1.Workspace) {
			r.ForceInitInterval = time.Nanosecond
		}},
		{name: "env changed", reason: "inputs changed", envs: map[string]string{"FOO": "baz"}},
		{name: "main.tf changed", reason: "inputs changed", mutate: func(t *testing.T, r *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			require.NoError(t, os.WriteFile(filepath.Join(r.Tf.GetWorkspacePath(ws), "main.tf"), []byte("changed"), 0644))
		}},
		{name: "another replica ran a newer init", reason: "differs from the last init by any replica", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Status.InitFingerprint = "from-another-pod"
		}},
		{name: "no fingerprint in status (upgrade from older operator)", reason: "differs from the last init by any replica", mutate: func(_ *testing.T, _ *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			ws.Status.InitFingerprint = ""
		}},
		{name: ".terraform removed", reason: "cannot verify installed modules", mutate: func(t *testing.T, r *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			require.NoError(t, os.RemoveAll(filepath.Join(r.Tf.GetWorkspacePath(ws), ".terraform")))
		}},
		{name: "nested floating module", reason: "nested module", mutate: func(t *testing.T, r *WorkspaceReconciler, ws *tfv1alphav1.Workspace) {
			wsPath := r.Tf.GetWorkspacePath(ws)
			nested := `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"my-module","Source":"` + pinned + `","Dir":".terraform/modules/my-module"},{"Key":"my-module.x","Source":"git::https://github.com/LEGO/x.git","Dir":".terraform/modules/my-module"}]}`
			require.NoError(t, os.WriteFile(filepath.Join(wsPath, ".terraform", "modules", "modules.json"), []byte(nested), 0644))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ws := setup(t)
			if tt.mutate != nil {
				tt.mutate(t, r, ws)
			}
			e := envs
			if tt.envs != nil {
				e = tt.envs
			}
			skip, reason := r.initSkipDecision(ws, e, tt.force)
			assert.False(t, skip)
			assert.Contains(t, reason, tt.reason)
		})
	}
}

func TestSkipInitReconciliation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))

	ws := newWs("test-skip-init", pinnedGitModule(t))
	ws.Generation = 1
	ws.Spec.AutoApply = false
	ws.Spec.ProviderSpecs = []tfv1alphav1.ProviderSpec{{Name: "random", Version: "3.7.2", Source: "hashicorp/random"}}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ws.Namespace},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	ws.Spec.TFExec = &tfv1alphav1.TFSpec{Env: []tfv1alphav1.EnvVar{
		{Name: "TEST_PASSWORD", SecretKeyRef: &tfv1alphav1.SecretKeySelector{Name: "creds", Key: "password"}},
	}}

	c := clientfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ws, &tfv1alphav1.Plan{}).WithObjects(ws, secret).Build()
	rootDir := t.TempDir()
	r := &WorkspaceReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(100),
		Tf:       runner.New(rootDir),
		Renderer: render.NewFileRender(rootDir),
		leases:   &sync.Map{},
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ws)}
	get := func() *tfv1alphav1.Workspace {
		var got tfv1alphav1.Workspace
		require.NoError(t, c.Get(t.Context(), req.NamespacedName, &got))
		return &got
	}
	reconcileRefresh := func() *tfv1alphav1.Workspace {
		cur := get()
		old := cur.DeepCopy()
		cur.Status.NextRefreshTimestamp = metav1.NewTime(time.Now().Add(-time.Second))
		require.NoError(t, c.Status().Patch(t.Context(), cur, client.MergeFrom(old)))
		_, err := r.Reconcile(t.Context(), req)
		require.NoError(t, err)
		return get()
	}
	planOutputEvents := func() int {
		n := 0
		for {
			select {
			case e := <-r.Recorder.(*record.FakeRecorder).Events:
				if strings.Contains(e, TFPlanEventReason) {
					n++
				}
			default:
				return n
			}
		}
	}

	_, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	first := get()
	require.Equal(t, TFPhaseCompleted, first.Status.TerraformPhase, first.Status.TerraformMessage)
	assert.Contains(t, first.Status.InitOutput, "Terraform has been successfully initialized")
	assert.NotEmpty(t, first.Status.CurrentContentHash)
	assert.Equal(t, 1, planOutputEvents(), "first reconcile plans")

	second := reconcileRefresh()
	require.Equal(t, TFPhaseCompleted, second.Status.TerraformPhase, second.Status.TerraformMessage)
	assert.Contains(t, second.Status.InitOutput, "init skipped")
	assert.Equal(t, first.Status.CurrentContentHash, second.Status.CurrentContentHash, "content hash is kept when init is skipped")
	assert.Equal(t, 1, planOutputEvents(), "plan still runs when init is skipped, so out-of-band changes are detected")

	var currentSecret v1.Secret
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(secret), &currentSecret))
	currentSecret.Data["password"] = []byte("rotated")
	require.NoError(t, c.Update(t.Context(), &currentSecret))
	rotated := reconcileRefresh()
	assert.Contains(t, rotated.Status.InitOutput, "Terraform has been successfully initialized", "rewriting a referenced secret forces a full init")
	assert.Equal(t, 1, planOutputEvents())

	_, err, ret, _, fingerprintEnvs := r.setupTerraformForWorkspace(t.Context(), get())
	require.NoError(t, err)
	require.False(t, ret)
	assert.NotContains(t, fingerprintEnvs["TEST_PASSWORD"], "rotated")
	assert.Contains(t, fingerprintEnvs["TEST_PASSWORD"], "creds/password")

	cur := get()
	cur.Spec.Module.Inputs = nil
	cur.Spec.TerraformRC = "# changed"
	cur.Generation = 2
	require.NoError(t, c.Update(t.Context(), cur))

	third := reconcileRefresh()
	require.Equal(t, TFPhaseCompleted, third.Status.TerraformPhase, third.Status.TerraformMessage)
	assert.Contains(t, third.Status.InitOutput, "Terraform has been successfully initialized")
	assert.Equal(t, 1, planOutputEvents())

	fourth := reconcileRefresh()
	assert.Contains(t, fourth.Status.InitOutput, "init skipped")
	assert.Equal(t, 1, planOutputEvents())
}

// Each reconciler simulates a replica with its own local workspace directory.
func TestSkipInitAcrossPods(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))

	ws := newWs("test-skip-init-pods", pinnedGitModule(t))
	ws.Generation = 1
	ws.Spec.AutoApply = false
	ws.Spec.ProviderSpecs = []tfv1alphav1.ProviderSpec{{Name: "random", Version: "3.7.2", Source: "hashicorp/random"}}

	c := clientfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ws, &tfv1alphav1.Plan{}).WithObjects(ws).Build()
	newPod := func() *WorkspaceReconciler {
		rootDir := t.TempDir()
		return &WorkspaceReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(100),
			Tf:       runner.New(rootDir),
			Renderer: render.NewFileRender(rootDir),
			leases:   &sync.Map{},
		}
	}
	podA, podB := newPod(), newPod()

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ws)}
	get := func() *tfv1alphav1.Workspace {
		var got tfv1alphav1.Workspace
		require.NoError(t, c.Get(t.Context(), req.NamespacedName, &got))
		return &got
	}
	refreshOn := func(r *WorkspaceReconciler) *tfv1alphav1.Workspace {
		cur := get()
		old := cur.DeepCopy()
		cur.Status.NextRefreshTimestamp = metav1.NewTime(time.Now().Add(-time.Second))
		require.NoError(t, c.Status().Patch(t.Context(), cur, client.MergeFrom(old)))
		_, err := r.Reconcile(t.Context(), req)
		require.NoError(t, err)
		got := get()
		require.Equal(t, TFPhaseCompleted, got.Status.TerraformPhase, got.Status.TerraformMessage)

		planned := false
		for done := false; !done; {
			select {
			case e := <-r.Recorder.(*record.FakeRecorder).Events:
				planned = planned || strings.Contains(e, TFPlanEventReason+" Terraform plan completed")
			default:
				done = true
			}
		}
		require.True(t, planned, "every reconciliation must run a successful plan")
		return got
	}
	const fullInit = "Terraform has been successfully initialized"

	got := refreshOn(podA)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod A: first reconcile")

	_, err := os.Stat(filepath.Join(podB.Tf.GetWorkspacePath(ws), ".terraform"))
	require.True(t, os.IsNotExist(err))
	got = refreshOn(podB)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod B: no local state, must run a full init")

	got = refreshOn(podA)
	assert.Contains(t, got.Status.InitOutput, "init skipped", "pod A: local state still valid")

	podB.ForceInitInterval = time.Nanosecond
	got = refreshOn(podB)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod B: force init interval elapsed")
	podB.ForceInitInterval = 0
	cur := get()
	old := cur.DeepCopy()
	cur.Status.InitFingerprint = "fingerprint-of-upgraded-lock-file-on-pod-b"
	require.NoError(t, c.Status().Patch(t.Context(), cur, client.MergeFrom(old)))
	got = refreshOn(podA)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod A: another replica has a newer installation")
	got = refreshOn(podA)
	assert.Contains(t, got.Status.InitOutput, "init skipped", "pod A: back in sync")

	cur = get()
	cur.Spec.TerraformRC = "# changed"
	cur.Generation = 2
	require.NoError(t, c.Update(t.Context(), cur))
	got = refreshOn(podB)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod B: spec changed")

	got = refreshOn(podA)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod A: local init is for an older generation")

	require.NoError(t, os.RemoveAll(filepath.Join(podA.Tf.GetWorkspacePath(ws), ".terraform")))
	got = refreshOn(podA)
	assert.Contains(t, got.Status.InitOutput, fullInit, "pod A: modules missing locally")
}

func TestGetEnvsForExecutionSecretDescriptors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, tfv1alphav1.AddToScheme(scheme))

	const secretValue = "super-secret-value"
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default", UID: "uid-1"},
		Data:       map[string][]byte{"password": []byte(secretValue)},
	}
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := &WorkspaceReconciler{Client: c, Scheme: scheme, Tf: runner.New(t.TempDir())}

	ws := newWs("secret-envs", "some-source")
	ws.Spec.TFExec = &tfv1alphav1.TFSpec{Env: []tfv1alphav1.EnvVar{
		{Name: "PLAIN", Value: "plain-value"},
		{Name: "PASSWORD", SecretKeyRef: &tfv1alphav1.SecretKeySelector{Name: "creds", Key: "password"}},
	}}

	envs, descriptors, err := r.getEnvsForExecution(t.Context(), ws)
	require.NoError(t, err)
	assert.Equal(t, secretValue, envs["PASSWORD"])
	assert.Equal(t, "plain-value", envs["PLAIN"])
	require.Contains(t, descriptors, "PASSWORD")
	assert.NotContains(t, descriptors, "PLAIN")
	assert.NotContains(t, descriptors["PASSWORD"], secretValue)
	assert.Contains(t, descriptors["PASSWORD"], "creds/password")
	assert.Contains(t, descriptors["PASSWORD"], "uid-1")

	var current v1.Secret
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(secret), &current))
	current.Data["password"] = []byte("rotated")
	require.NoError(t, c.Update(t.Context(), &current))

	_, rotated, err := r.getEnvsForExecution(t.Context(), ws)
	require.NoError(t, err)
	assert.NotEqual(t, descriptors["PASSWORD"], rotated["PASSWORD"], "descriptor must change when the secret is written")
}
