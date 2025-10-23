package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tfv1alphav1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	authv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	DebugLevel = 2

	defaultPlanHistoryLimit = 3
	planCreationTimeout     = 2 * time.Second

	TFErrEventReason      = "TerraformError"
	TFPlanEventReason     = "TerraformPlan"
	TFApplyEventReason    = "TerraformApply"
	TFDestroyEventReason  = "TerraformDestroy"
	TFValidateEventReason = "TerraformValidate"

	// Terraform execution phases
	TFPhaseIdle      = ""
	TFPhasePlanning  = "Planning"
	TFPhaseApplying  = "Applying"
	TFPhaseCompleted = "Completed"
	TFPhaseErrored   = "Errored"
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Tf       *runner.Exec
	Renderer render.Renderer
}

func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	ws := &tfv1alphav1.Workspace{}
	if err := r.Client.Get(ctx, req.NamespacedName, ws); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get workspace %s: %w", req.String(), err)
		}
		return ctrl.Result{}, nil
	}
	logf.IntoContext(ctx, log.WithValues("workspace", req.String()))
	if ws.Status.ObservedGeneration == ws.Generation && time.Now().Before(ws.Status.NextRefreshTimestamp.Time) && !ws.ManualApplyRequested() {
		return ctrl.Result{RequeueAfter: time.Until(ws.Status.NextRefreshTimestamp.Time)}, nil
	}

	res, err, ret, tf := r.setupTerraformForWorkspace(ctx, ws)
	if ret {
		return res, fmt.Errorf("setupTerraformForWorkspace: %w", err)
	}

	if res, err, ret = r.handleRendering(ctx, ws); ret {
		return res, fmt.Errorf("handleRendering: %w", err)
	}

	if res, err, ret = r.handleRefreshDependencies(ctx, ws, tf); ret {
		return res, fmt.Errorf("handleRefreshDependencies: %w", err)
	}

	if res, err, ret = r.handleDeletionAndFinalizers(ctx, ws, tf); ret {
		return res, fmt.Errorf("handleDeletionAndFinalizers: %w", err)
	}

	if res, err, ret = r.handlePlan(ctx, ws, tf); ret {
		return res, fmt.Errorf("handlePlan: %w", err)
	}

	if res, err, ret = r.handleApply(ctx, ws, tf); ret {
		return res, fmt.Errorf("handleApply: %w", err)
	}

	if res, err, ret = r.handleReschedule(ctx, ws); ret {
		return res, fmt.Errorf("handleReschedule: %w", err)
	}

	log.V(DebugLevel).Info("reconcile completed")
	return ctrl.Result{RequeueAfter: time.Until(ws.Status.NextRefreshTimestamp.Time)}, nil
}

func (r *WorkspaceReconciler) handleRendering(ctx context.Context, ws *tfv1alphav1.Workspace) (ctrl.Result, error, bool) {
	rendering, err := r.Renderer.Render(ws)
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to render workspace: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	old := ws.DeepCopy()
	ws.Status.CurrentRender = rendering
	err = r.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update workspace status after rendering: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) handleRefreshDependencies(ctx context.Context, ws *tfv1alphav1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	log.V(DebugLevel).Info("refresh dependencies starting")
	defer log.V(DebugLevel).Info("refresh dependencies completed")

	err := r.Tf.TerraformInit(ctx, tf, func(stdout, stderr string) {
		old := ws.DeepCopy()
		ws.Status.InitOutput, _ = constructOutput(stdout, stderr, nil)

		err := r.Status().Patch(ctx, ws, client.MergeFrom(old))
		if err != nil {
			_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update workspace init output: %v", err), nil)
			return
		}
	})
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to init terraform: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	valResult, err := tf.Validate(ctx)
	if err != nil {
		log.Error(err, "failed to validate terraform", "workspace", ws.Name)
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to validate terraform: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	r.Recorder.Eventf(ws, v1.EventTypeNormal, TFValidateEventReason, "Terraform validation completed, valid: %t", valResult.Valid)

	old := ws.DeepCopy()
	ws.Status.ValidRender = valResult.Valid
	err = r.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		return ctrl.Result{}, err, true
	}

	if !valResult.Valid {
		log.Error(err, "Terraform validation failed", "workspace", ws.Name)
		diagnosticsMsg := r.formatValidationDiagnostics(valResult.Diagnostics)
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, diagnosticsMsg, nil)
		return ctrl.Result{}, fmt.Errorf("terraform validation failed: %s", diagnosticsMsg), true
	}

	sum, err := r.Tf.CalculateChecksum(ws)
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to calculate dependency hash: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	workspaceUnchanged := ws.Status.ObservedGeneration == ws.Generation
	checksumUnchanged := ws.Status.CurrentContentHash == sum && ws.Status.CurrentContentHash != ""
	hasPlan := ws.Status.CurrentPlan != nil
	isDeleting := !ws.DeletionTimestamp.IsZero()

	if isDeleting {
		log.V(DebugLevel).Info("workspace is being deleted, skipping unchanged checks")
		return ctrl.Result{}, nil, false
	}

	if ws.ManualApplyRequested() && hasPlan {
		log.V(DebugLevel).Info("manual apply requested, marking apply needed")
		old = ws.DeepCopy()
		ws.Status.NewApplyNeeded = true

		err = r.Status().Patch(ctx, ws, client.MergeFrom(old))
		if err != nil {
			_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update workspace status for manual apply: %v", err), nil)
			return ctrl.Result{}, err, true
		}

		return ctrl.Result{}, nil, false
	}

	if workspaceUnchanged && checksumUnchanged && hasPlan {
		log.V(DebugLevel).Info("skipping refresh - no changes detected")
		return ctrl.Result{}, nil, false
	}

	old = ws.DeepCopy()
	ws.Status.CurrentContentHash = sum
	ws.Status.NewPlanNeeded = true

	err = r.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update dependency hash label: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) handlePlan(ctx context.Context, ws *tfv1alphav1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	if !ws.Status.NewPlanNeeded {
		return ctrl.Result{}, nil, false
	}

	log.V(DebugLevel).Info("handle plan starting")
	defer log.V(DebugLevel).Info("handle plan completed")

	_ = r.updateWorkspaceStatus(ctx, ws, TFPhasePlanning, "Starting terraform plan", nil)

	changed, planOutput, err := r.executeTerraformPlan(ctx, tf, false, func(stdout, stderr string) {
		old := ws.DeepCopy()
		ws.Status.LastPlanOutput, _ = constructOutput(stdout, stderr, nil)

		err := r.Status().Patch(ctx, ws, client.MergeFrom(old))
		if err != nil {
			_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update workspace plan output: %v", err), nil)
			return
		}
	})

	if err != nil {
		log.Error(err, "failed to execute terraform plan", "workspace", ws.Name)
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to execute terraform plan: %v", err), nil)
		return ctrl.Result{}, err, true
	}
	ws.Status.LastPlanOutput = ""

	if !changed {
		log.Info("plan has no changes, marking as completed", "workspace", ws.Name)
		now := metav1.Now()
		err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Plan completed - no changes needed", func(s *tfv1alphav1.WorkspaceStatus) {
			s.HasChanges = false
			s.LastExecutionTime = &now
			s.LastPlanOutput = ""
		})
		if err != nil {
			return ctrl.Result{}, err, true
		}
	}

	plan, err := r.createPlanRecord(ctx, ws, changed, planOutput, "", tfv1alphav1.PlanPhasePlanned, false)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create plan record: %w", err), true

	}

	err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Plan completed", func(s *tfv1alphav1.WorkspaceStatus) {
		s.HasChanges = changed
		s.NewPlanNeeded = false
		s.NewApplyNeeded = true
		s.LastPlanOutput = ""
		s.CurrentPlan = &tfv1alphav1.PlanReference{
			Name:      plan.Name,
			Namespace: plan.Namespace,
		}
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status with current plan: %w", err), true
	}

	r.Recorder.Eventf(ws, v1.EventTypeNormal, TFPlanEventReason, "Terraform plan completed, changes: %t", changed)

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) handleApply(ctx context.Context, ws *tfv1alphav1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	if !ws.Status.NewApplyNeeded && !ws.ManualApplyRequested() {
		return ctrl.Result{}, nil, false
	}

	log.V(DebugLevel).Info("handle apply starting")
	defer log.V(DebugLevel).Info("handle apply completed")

	if !ws.Spec.AutoApply && !ws.ManualApplyRequested() {
		now := metav1.Now()
		r.Recorder.Eventf(ws, v1.EventTypeNormal, TFApplyEventReason, "Auto-apply is disabled, skipping apply")
		err := r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Apply skipped, no Auto-apply enabled", func(s *tfv1alphav1.WorkspaceStatus) {
			s.LastExecutionTime = &now
			s.NewApplyNeeded = false
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update workspace status after skipping apply: %w", err), true
		}

		return ctrl.Result{}, nil, false
	}

	if ws.Status.HasChanges {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseApplying, "Applying terraform changes", nil)

		applyOutput, err := r.executeTerraformApply(ctx, tf, false, func(stdout, stderr string) {
			old := ws.DeepCopy()
			ws.Status.LastApplyOutput, _ = constructOutput(stdout, stderr, nil)

			err := r.Status().Patch(ctx, ws, client.MergeFrom(old))
			if err != nil {
				_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update workspace plan output: %v", err), nil)
				return
			}
		})
		if err != nil {
			log.Error(err, "failed to apply terraform", "workspace", ws.Name)
			_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to apply terraform: %v", err), func(s *tfv1alphav1.WorkspaceStatus) {
				s.LastApplyOutput = applyOutput
			})

			return ctrl.Result{}, err, true
		}

		_, err = r.createPlanRecord(ctx, ws, ws.Status.HasChanges, ws.Status.LastPlanOutput, applyOutput, tfv1alphav1.PlanPhaseApplied, false)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create plan record after failed apply: %w", err), true
		}

		now := metav1.Now()
		err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Apply completed successfully", func(s *tfv1alphav1.WorkspaceStatus) {
			s.LastExecutionTime = &now
			s.LastApplyOutput = applyOutput
		})

		if ws.ManualApplyRequested() {
			r.Recorder.Eventf(ws, v1.EventTypeNormal, TFApplyEventReason, "Manual terraform apply completed successfully")
		} else {
			r.Recorder.Eventf(ws, v1.EventTypeNormal, TFApplyEventReason, "Terraform apply completed successfully")
		}
	}

	old := ws.DeepCopy()
	ws.Status.NewApplyNeeded = false
	err := r.Client.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status after apply: %w", err), true
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) setupTerraformForWorkspace(ctx context.Context, ws *tfv1alphav1.Workspace) (ctrl.Result, error, bool, *tfexec.Terraform) {
	log := logf.FromContext(ctx)
	defer log.V(DebugLevel).Info("setup terraform completed")
	log.V(DebugLevel).Info("setup terraform starting")

	envs, err := r.getEnvsForExecution(ctx, ws)
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to get envs for execution: %v", err), nil)
		return ctrl.Result{}, err, true, nil
	}

	tf, terraformRCPath, err := r.Tf.GetTerraformForWorkspace(ctx, ws)
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to get terraform executable: %v", err), nil)
		return ctrl.Result{}, err, true, nil
	}

	envs["HOME"] = os.Getenv("HOME")
	envs["PATH"] = os.Getenv("PATH")
	envs["TF_PLUGIN_CACHE_DIR"] = r.Tf.PluginCacheDir
	envs["TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE"] = "true"

	if terraformRCPath != "" {
		envs["TF_CLI_CONFIG_FILE"] = terraformRCPath
	}

	err = tf.SetEnv(envs)
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to set terraform env: %v", err), nil)
		return ctrl.Result{}, err, true, nil
	}

	return ctrl.Result{}, nil, false, tf
}

func (r *WorkspaceReconciler) handleDeletionAndFinalizers(ctx context.Context, ws *tfv1alphav1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	if ws.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(ws, tfv1alphav1.WorkspaceFinalizer) {
		controllerutil.AddFinalizer(ws, tfv1alphav1.WorkspaceFinalizer)
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err), true
		}
	}

	if ws.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil, false
	}

	log.V(DebugLevel).Info("handling deletion and finalizers starting")
	defer log.V(DebugLevel).Info("handling deletion and finalizers completed")

	if !ws.Spec.PreventDestroy {
		err := tf.Destroy(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to destroy terraform resources: %w", err), true
		}
	}

	err := r.Tf.CleanupWorkspace(ws)
	if err != nil {
		log.Error(err, "failed to cleanup workspace directory, but proceeding with finalization")
	}

	updated := controllerutil.RemoveFinalizer(ws, tfv1alphav1.WorkspaceFinalizer)
	if updated {
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err, true
		}
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) handleReschedule(ctx context.Context, ws *tfv1alphav1.Workspace) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	if !ws.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil, false
	}

	log.V(DebugLevel).Info("reshedule starting")
	defer log.V(DebugLevel).Info("reshedule completed")

	old := ws.DeepCopy()

	delete(ws.Annotations, tfv1alphav1.ManualApplyAnnotation)
	err := r.Client.Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch workspace during cleanup: %w", err), true
	}

	old = ws.DeepCopy()
	ws.Status.ObservedGeneration = ws.Generation
	ws.Status.NextRefreshTimestamp = metav1.NewTime(time.Now().Add(5 * time.Minute))
	err = r.Client.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch workspace status during cleanup: %w", err), true
	}

	err = r.cleanupOldPlans(ctx, ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cleanup old plans: %w", err), true
	}

	return ctrl.Result{}, nil, false
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfv1alphav1.Workspace{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}). // Match terraform execution capacity
		Complete(r)
}

func (r *WorkspaceReconciler) executeTerraformPlan(ctx context.Context, tf *tfexec.Terraform, destroy bool, cb func(stdout, stderr string)) (bool, string, error) {
	var changed bool
	var err error

	r.Tf.WithOutputStream(ctx, tf, func() {
		changed, err = tf.Plan(ctx, tfexec.Destroy(destroy), tfexec.Out("plan.out"))
	}, cb)

	if err != nil {
		return false, "", fmt.Errorf("failed to plan terraform: %w", err)
	}

	planOutput, err := tf.ShowPlanFileRaw(ctx, "plan.out")
	if err != nil {
		return false, "", fmt.Errorf("failed to show plan file: %w", err)
	}

	return changed, planOutput, nil
}

// executeTerraformApply executes terraform apply or destroy command
func (r *WorkspaceReconciler) executeTerraformApply(ctx context.Context, tf *tfexec.Terraform, destroy bool, cb func(stdout, stderr string)) (string, error) {
	var stdout, stderr string
	var err error
	r.Tf.WithOutputStream(ctx, tf, func() {
		if destroy {
			err = tf.Destroy(ctx)
		} else {
			err = tf.Apply(ctx)
		}
	}, func(so, se string) {
		stdout = so
		stderr = se
		cb(stdout, stderr)
	})

	return constructOutput(stdout, stderr, err)
}

func constructOutput(stdout, stderr string, err error) (string, error) {
	var output string
	if len(stdout) > 0 {
		output += "=== TERRAFORM OUTPUT ===\n"
		output += stdout
	}

	if len(stderr) > 0 {
		if output != "" {
			output += "\n"
		}
		output += "=== TERRAFORM DIAGNOSTICS ===\n"
		output += stderr
	}

	if err != nil && output != "" {
		output += "\n=== ERROR DETAILS ===\n"
		output += fmt.Sprintf("Exit error: %v", err)
	}

	if output == "" && err != nil {
		output = fmt.Sprintf("Terraform command failed: %v", err)
	}

	return output, err
}

// updateWorkspaceStatus updates the terraform execution status in the workspace
func (r *WorkspaceReconciler) updateWorkspaceStatus(ctx context.Context, ws *tfv1alphav1.Workspace, phase string, message string, updater func(status *tfv1alphav1.WorkspaceStatus)) error {
	old := ws.DeepCopy()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ws), ws); err != nil {
			return err
		}

		ws.Status.TerraformPhase = phase
		ws.Status.TerraformMessage = message

		if updater != nil {
			updater(&ws.Status)
		}

		return r.Client.Status().Patch(ctx, ws, client.MergeFrom(old))
	})
	return err
}

// createPlanRecord creates a Plan CRD as an audit record after terraform execution
func (r *WorkspaceReconciler) createPlanRecord(ctx context.Context, ws *tfv1alphav1.Workspace, hasChanges bool, planOutput, applyOutput string, phase tfv1alphav1.PlanPhase, destroy bool) (*tfv1alphav1.Plan, error) {
	planName := fmt.Sprintf("%s-%d", ws.Name, ws.Generation)

	var message string
	switch phase {
	case tfv1alphav1.PlanPhasePlanned:
		message = "Plan completed"
	case tfv1alphav1.PlanPhaseErrored:
		// We only create a plan object if a plan was successfully run, so assume apply has failed
		message = "Plan completed but apply failed"
	case tfv1alphav1.PlanPhasePlanning:
		message = "Plan in progress"
	case tfv1alphav1.PlanPhaseApplied:
		message = "Plan completed and applied"
	case tfv1alphav1.PlanPhaseCancelled:
		// We only create a plan object if a plan was successfully run, so assume apply was cancelled
		message = "Plan completed but apply cancelled"
	}

	now := metav1.Now()
	plan := &tfv1alphav1.Plan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      planName,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				tfv1alphav1.WorkspacePlanLabel: ws.Name,
			},
		},
		Spec: tfv1alphav1.PlanSpec{
			WorkspaceRef: tfv1alphav1.WorkspaceReference{
				Name:      ws.Name,
				Namespace: ws.Namespace,
			},
			AutoApply:        ws.Spec.AutoApply,
			TerraformVersion: ws.Spec.TerraformVersion,
			Render:           ws.Status.CurrentRender,
			Destroy:          destroy,
		},
	}

	err := controllerutil.SetControllerReference(ws, plan, r.Scheme)
	if err != nil {
		return nil, fmt.Errorf("failed to set controller reference on plan: %w", err)
	}

	err = r.Client.Create(ctx, plan)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create plan audit record: %w", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, planCreationTimeout)
	defer cancel()
	waitErr := wait.PollUntilContextTimeout(ctxTimeout, 100*time.Millisecond, planCreationTimeout, true,
		func(ctx context.Context) (done bool, err error) {
			getErr := r.Client.Get(ctx, client.ObjectKeyFromObject(plan), plan)
			if getErr == nil {
				return true, nil
			}
			if apierrors.IsNotFound(getErr) {
				return false, nil
			}
			return false, getErr
		},
	)
	if waitErr != nil {
		return nil, fmt.Errorf("plan not found after creation: %w", waitErr)
	}

	// The plan status must be updated in a separate call
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
			return err
		}

		// Update the status with the latest outputs
		plan.Status = tfv1alphav1.PlanStatus{
			Phase:              phase,
			Message:            message,
			PlanOutput:         planOutput,
			ApplyOutput:        applyOutput,
			HasChanges:         hasChanges,
			ValidRender:        true,
			StartTime:          &now,
			CompletionTime:     &now,
			ObservedGeneration: 1,
		}

		return r.Client.Status().Update(ctx, plan)
	})

	if err != nil {
		return nil, fmt.Errorf("failed to update plan status: %w", err)
	}
	return plan, nil
}

// getEnvsForExecution gets environment variables for terraform execution
func (r *WorkspaceReconciler) getEnvsForExecution(ctx context.Context, ws *tfv1alphav1.Workspace) (map[string]string, error) {
	envs := make(map[string]string)

	if ws.Spec.TFExec != nil && ws.Spec.TFExec.Env != nil {
		for _, env := range ws.Spec.TFExec.Env {
			if env.Name == "" {
				continue
			}
			if env.Value != "" {
				envs[env.Name] = env.Value
				continue
			}
			if env.ConfigMapKeyRef != nil {
				var cm v1.ConfigMap
				err := r.Client.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: env.ConfigMapKeyRef.Name}, &cm)
				if err != nil {
					return nil, fmt.Errorf("failed to get configmap %s: %w", env.ConfigMapKeyRef.Name, err)
				}
				if val, ok := cm.Data[env.ConfigMapKeyRef.Key]; ok {
					envs[env.Name] = val
					continue
				}
			}
			if env.SecretKeyRef != nil {
				value, err := r.getSecretFromRef(ctx, ws, env.SecretKeyRef)
				if err != nil {
					return nil, fmt.Errorf("failed to get secret for env %s: %w", env.Name, err)
				}
				envs[env.Name] = value
				continue
			}
		}
	}

	if ws.Spec.Authentication != nil {
		if ws.Spec.Authentication.AWS != nil {
			if ws.Spec.Authentication.AWS.ServiceAccountName != "" || ws.Spec.Authentication.AWS.RoleARN != "" {
				tempTokenPath, err := r.setupAWSAuthentication(ctx, ws)
				if err != nil {
					return nil, fmt.Errorf("failed to setup AWS authentication: %w", err)
				}

				envs["AWS_WEB_IDENTITY_TOKEN_FILE"] = tempTokenPath
				envs["AWS_ROLE_ARN"] = ws.Spec.Authentication.AWS.RoleARN
			}
		}

		if ws.Spec.Authentication.Tokens != nil {
			wsPath, err := r.Tf.SetupWorkspace(ws)
			if err != nil {
				return nil, fmt.Errorf("failed to setup workspace for token files: %w", err)
			}
			for _, token := range ws.Spec.Authentication.Tokens {
				value, err := r.getSecretFromRef(ctx, ws, &token.SecretKeyRef)
				if err != nil {
					return nil, fmt.Errorf("failed to get token for env %s: %w", token.FilePathEnv, err)
				}

				tokenFilePath := filepath.Join(wsPath, fmt.Sprintf("%s-token", strings.ToLower(token.FilePathEnv)))
				err = os.WriteFile(tokenFilePath, []byte(value), 0600)
				if err != nil {
					return nil, fmt.Errorf("failed to write token file for env %s: %w", token.FilePathEnv, err)
				}

				envs[token.FilePathEnv] = tokenFilePath
			}
		}
	}

	return envs, nil
}

// setupAWSAuthentication creates temporary AWS token file for authentication
func (r *WorkspaceReconciler) setupAWSAuthentication(ctx context.Context, ws *tfv1alphav1.Workspace) (string, error) {
	var sa v1.ServiceAccount
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ws.Namespace,
		Name:      ws.Spec.Authentication.AWS.ServiceAccountName,
	}, &sa)
	if err != nil {
		return "", fmt.Errorf("failed to get service account (%s) in namespace (%s): %w",
			ws.Spec.Authentication.AWS.ServiceAccountName, ws.Namespace, err)
	}

	tokenRequest := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{"sts.amazonaws.com"},
			ExpirationSeconds: func(i int64) *int64 { return &i }(600),
		},
	}

	err = r.Client.SubResource("token").Create(ctx, &sa, tokenRequest)
	if err != nil {
		return "", fmt.Errorf("failed to create token for service account %s: %w",
			ws.Spec.Authentication.AWS.ServiceAccountName, err)
	}
	wsPath, err := r.Tf.SetupWorkspace(ws)
	if err != nil {
		return "", fmt.Errorf("failed to setup workspace for aws token file: %w", err)
	}

	path := filepath.Join(wsPath, "aws-token")
	err = os.WriteFile(path, []byte(tokenRequest.Status.Token), 0600)
	if err != nil {
		return "", fmt.Errorf("failed to create temp token file: %w", err)
	}

	return path, nil
}

// formatValidationDiagnostics formats terraform validation diagnostics into a detailed error message
func (r *WorkspaceReconciler) formatValidationDiagnostics(diagnostics []tfjson.Diagnostic) string {
	var b strings.Builder
	b.WriteString("Terraform validation failed with the following diagnostics:\n\n")
	for i, diag := range diagnostics {
		fmt.Fprintf(&b, "Diagnostic %d:\n", i+1)
		fmt.Fprintf(&b, "  Severity: %s\n", diag.Severity)
		fmt.Fprintf(&b, "  Summary: %s\n", diag.Summary)

		if diag.Detail != "" {
			fmt.Fprintf(&b, "  Detail: %s\n", diag.Detail)
		}
		if diag.Range != nil {
			fmt.Fprintf(&b, "  Location: %s:%d:%d\n",
				diag.Range.Filename, diag.Range.Start.Line, diag.Range.Start.Column)
		}
		if diag.Snippet != nil {
			if diag.Snippet.Context != nil {
				fmt.Fprintf(&b, "  Context: %s\n", *diag.Snippet.Context)
			}
			if diag.Snippet.Code != "" {
				fmt.Fprintf(&b, "  Code: %s\n", diag.Snippet.Code)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (r *WorkspaceReconciler) getSecretFromRef(ctx context.Context, ws *tfv1alphav1.Workspace, ref *tfv1alphav1.SecretKeySelector) (string, error) {
	var secret v1.Secret
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ref.Name}, &secret)
	if err != nil {
		return "", fmt.Errorf("failed to get secret %s: %w", ref.Name, err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s", ref.Key, ref.Name)
	}

	return string(value), nil
}

func (r *WorkspaceReconciler) cleanupOldPlans(ctx context.Context, ws *tfv1alphav1.Workspace) error {
	var planList tfv1alphav1.PlanList
	err := r.Client.List(ctx, &planList, client.InNamespace(ws.Namespace), client.MatchingLabels{
		tfv1alphav1.WorkspacePlanLabel: ws.Name,
	})
	if err != nil {
		return fmt.Errorf("failed to list plans for workspace %s: %w", ws.Name, err)
	}

	limit := defaultPlanHistoryLimit
	if ws.Spec.PlanHistoryLimit > 0 {
		limit = int(ws.Spec.PlanHistoryLimit)
	}

	if len(planList.Items) <= limit {
		return nil
	}

	plans := planList.Items
	sort.Slice(plans, func(i, j int) bool {
		return plans[i].CreationTimestamp.Time.Before(plans[j].CreationTimestamp.Time)
	})

	toDelete := plans[:len(plans)-int(ws.Spec.PlanHistoryLimit)]
	for _, plan := range toDelete {
		err := r.Client.Delete(ctx, &plan)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete old plan %s: %w", plan.Name, err)
		}
	}

	return nil
}
