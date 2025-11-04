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
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute*5)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{testutils.CRDFolder()},
		BinaryAssetsDirectory: testutils.GetFirstFoundEnvTestBinaryDir(),
		ErrorIfCRDPathMissing: true,
		Scheme:                k8sscheme.Scheme,
	}

	modHost, shutdown := testutils.NewModuleHost()

	err := tfv1alphav1.AddToScheme(testEnv.Scheme)
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

	rootDir := t.TempDir() // testutils.TestDataFolder() // Enable to better introspection into test data
	t.Logf("using root dir: %s", rootDir)
	err = (&WorkspaceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("krec"),

		Tf:       runner.New(rootDir),
		Renderer: render.NewFileRender(rootDir),
	}).SetupWithManager(mgr)

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
		ws.Spec.PreventDestroy = false
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
		assert.NotEmpty(t, relevantPlan.Status.ApplyOutput)

		assert.Len(t, reasons, 3)
		assert.Contains(t, reasons, TFApplyEventReason)
		assert.Contains(t, reasons, TFPlanEventReason)
		assert.Contains(t, reasons, TFValidateEventReason)
	})

	t.Run("manual apply request", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-manual-apply", modHost.ModuleSource("my-module"))
		ws.Spec.PreventDestroy = false
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
		ws.Spec.PreventDestroy = false
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

	t.Run("cleanup plans on deletion", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-resource-cleanup", modHost.ModuleSource("my-module"))
		ws.Spec.PreventDestroy = false
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

		plans := &tfv1alphav1.PlanList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(plans, 1, plansForWs(ws)), wait.WithContext(ctx))

		assert.Len(t, plans.Items, 1)
		assert.Equal(t, 2, int(ws.Generation))
		assert.Equal(t, fmt.Sprintf("%s-2", ws.Name), plans.Items[0].Name)
	})

	t.Run("error status and events are preserved", func(t *testing.T) {
		t.Parallel()
		ws := newWs("test-error-preservation", "http://invalid-source-that-does-not-exist")
		ws.Spec.PreventDestroy = false
		ws.Spec.AutoApply = false
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			ws := object.(*tfv1alphav1.Workspace)
			return ws.Status.TerraformPhase == TFPhaseErrored
		}), wait.WithTimeout(time.Second*30))
		assert.NoError(t, err)

		firstErrorMessage := ws.Status.TerraformMessage
		firstErrorTime := ws.Status.LastErrorTime
		assert.NotEmpty(t, firstErrorMessage)
		assert.NotNil(t, firstErrorTime)
		assert.Equal(t, firstErrorMessage, ws.Status.LastErrorMessage)

		time.Sleep(time.Second * 2)

		assert.NoError(t, k.Resources().Get(ctx, ws.Name, ws.Namespace, ws))

		assert.NotNil(t, ws.Status.LastErrorTime)
		assert.NotEmpty(t, ws.Status.LastErrorMessage)
		assert.Equal(t, firstErrorTime.Unix(), ws.Status.LastErrorTime.Unix())
		assert.Contains(t, ws.Status.LastErrorMessage, "Failed to")

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 0, testutils.EventOwnedBy(ws.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)

		var errorEvents []v1.Event
		for _, e := range events.Items {
			if e.Type == v1.EventTypeWarning && e.Reason == TFErrEventReason {
				errorEvents = append(errorEvents, e)
			}
		}
		assert.NotEmpty(t, errorEvents, "Expected at least one error event")
		assert.Contains(t, errorEvents[0].Message, "Failed to")
	})

	t.Run("validation error creates event and preserves error", func(t *testing.T) {
		t.Parallel()

		invalidModule := `resource "invalid_resource" "test" {
		# Missing required argument
		invalid_syntax here
		}`
		modHost.AddFileToModule("invalid-module", "main.tf", invalidModule)

		ws := newWs("test-validation-error", modHost.ModuleSource("invalid-module"))
		ws.Spec.PreventDestroy = false
		ws.Spec.AutoApply = false
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			ws := object.(*tfv1alphav1.Workspace)
			return ws.Status.TerraformPhase == TFPhaseErrored && !ws.Status.ValidRender
		}), wait.WithTimeout(time.Second*30))
		assert.NoError(t, err)

		events := &v1.EventList{}
		err = wait.For(conditions.New(k.Resources()).ResourceListN(events, 0, testutils.EventOwnedBy(ws.Name)), wait.WithContext(ctx))
		assert.NoError(t, err)

		var validationEvents []v1.Event
		for _, e := range events.Items {
			if e.Reason == TFValidateEventReason || e.Reason == TFErrEventReason {
				validationEvents = append(validationEvents, e)
			}
		}
		assert.NotEmpty(t, validationEvents, "Expected validation error events")

		// Verify error status fields
		assert.NotNil(t, ws.Status.LastErrorTime)
		assert.NotEmpty(t, ws.Status.LastErrorMessage)
		assert.Contains(t, ws.Status.LastErrorMessage, "validation")
	})

	t.Run("successful reconciliation after error clears current message but preserves last error", func(t *testing.T) {
		t.Parallel()

		validModule := `variable "pet_length" {
		default = 2
		type    = number
		}

		resource "random_pet" "test" {
		length = var.pet_length
		}`
		modHost.AddFileToModule("fix-module", "main.tf", validModule)

		ws := newWs("test-error-recovery", modHost.ModuleSource("fix-module"))
		ws.Spec.PreventDestroy = false
		ws.Spec.AutoApply = true
		assert.NoError(t, k.Resources().Create(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			ws := object.(*tfv1alphav1.Workspace)
			return ws.Status.TerraformPhase == TFPhaseCompleted
		}), wait.WithTimeout(time.Second*30))
		assert.NoError(t, err)

		invalidModule := `variable "pet_length" {
		default = 2
		type    = number
		}

		resource "random_pet" "test" {
		# Missing required length argument
		}`
		modHost.AddFileToModule("fix-module", "main.tf", invalidModule)

		ws.Spec.Module.Inputs = testutils.Json(map[string]interface{}{
			"pet_length": 3,
		})
		assert.NoError(t, k.Resources().Update(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			ws := object.(*tfv1alphav1.Workspace)
			return ws.Status.TerraformPhase == TFPhaseErrored
		}), wait.WithTimeout(time.Second*30))
		assert.NoError(t, err)

		lastErrorMessage := ws.Status.LastErrorMessage
		lastErrorTime := ws.Status.LastErrorTime
		assert.NotEmpty(t, lastErrorMessage)
		assert.NotNil(t, lastErrorTime)

		modHost.AddFileToModule("fix-module", "main.tf", validModule)

		ws.Spec.Module.Inputs = testutils.Json(map[string]interface{}{
			"pet_length": 4,
		})
		assert.NoError(t, k.Resources().Update(ctx, ws))

		err = wait.For(conditions.New(k.Resources()).ResourceMatch(ws, func(object k8s.Object) bool {
			ws := object.(*tfv1alphav1.Workspace)
			return ws.Status.TerraformPhase == TFPhaseCompleted && ws.Status.ObservedGeneration == ws.Generation
		}), wait.WithTimeout(time.Second*45))
		assert.NoError(t, err)

		assert.NotNil(t, ws.Status.LastErrorTime)
		assert.NotEmpty(t, ws.Status.LastErrorMessage)
		assert.Equal(t, lastErrorTime.Unix(), ws.Status.LastErrorTime.Unix())
		assert.Equal(t, lastErrorMessage, ws.Status.LastErrorMessage)

		assert.NotEqual(t, ws.Status.TerraformMessage, ws.Status.LastErrorMessage)
		assert.NotContains(t, ws.Status.TerraformMessage, "Failed to")
	})
}

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
			AutoApply:        true,
			PreventDestroy:   true,
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
				Source: moduleSource,
				Name:   "my-module",
			},
		},
	}
}
