package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
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
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	DebugLevel = 0

	TFErrEventReason      = "TerraformError"
	TFPlanEventReason     = "TerraformPlan"
	TFApplyEventReason    = "TerraformApply"
	TFDestroyEventReason  = "TerraformDestroy"
	TFValidateEventReason = "TerraformValidate"

	workspaceFinalizer = "tf-reconcile.lego.com/finalizer"

	// Terraform execution phases
	TFPhaseIdle      = ""
	TFPhasePlanning  = "Planning"
	TFPhaseApplying  = "Applying"
	TFPhaseCompleted = "Completed"
	TFPhaseErrored   = "Errored"

	// PlanHistoryLimit defines how many old plans to keep as audit records
	PlanHistoryLimit = 3

	workspacePlanLabel = "tf-reconcile.lego.com/workspace"
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
	//reqStart := time.Now()
	ws := &tfreconcilev1alpha1.Workspace{}
	if err := r.Client.Get(ctx, req.NamespacedName, ws); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get workspace %s: %w", req.String(), err)
		}
		return ctrl.Result{}, nil
	}
	logf.IntoContext(ctx, log.WithValues("workspace", req.String()))
	if ws.Status.ObservedGeneration == ws.Generation && time.Now().Before(ws.Status.NextRefreshTimestamp.Time) {
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

	if res, err, ret = r.handleCleanup(ctx, ws); ret {
		return res, fmt.Errorf("handleCleanup: %w", err)
	}

	log.V(DebugLevel).Info("reconcile completed")
	return ctrl.Result{RequeueAfter: time.Until(ws.Status.NextRefreshTimestamp.Time)}, nil
}

func (r *WorkspaceReconciler) handleRendering(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (ctrl.Result, error, bool) {
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

func (r *WorkspaceReconciler) handleRefreshDependencies(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	defer log.V(DebugLevel).Info("refresh dependencies completed")
	log.V(DebugLevel).Info("refreshing dependencies starting")

	err := r.Tf.TerraformInit(ctx, tf)
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
	checksumChanged := ws.Status.CurrentContentHash != sum && ws.Status.CurrentContentHash != ""
	//timeForRefresh := t.After(ws.Status.NextRefreshTimestamp.Time)
	missingPlan := ws.Status.CurrentPlan == nil

	if workspaceUnchanged && !checksumChanged && /*!timeForRefresh &&*/ !missingPlan {
		log.Info("skipping refresh - no changes detected", "workspace", ws.Name)
		return ctrl.Result{}, nil, false
	}

	old = ws.DeepCopy()
	ws.Status.CurrentContentHash = sum
	ws.Status.NewPlanNeeded = true
	ws.Status.NewApplyNeeded = true

	err = r.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to update dependency hash label: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) handlePlan(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	defer log.V(DebugLevel).Info("handle plan completed")
	log.V(DebugLevel).Info("handle plan starting")

	if !ws.Status.NewPlanNeeded {
		return ctrl.Result{}, nil, false
	}

	_ = r.updateWorkspaceStatus(ctx, ws, TFPhasePlanning, "Starting terraform plan", nil)

	changed, planOutput, err := r.executeTerraformPlan(ctx, tf, false)
	if err != nil {
		log.Error(err, "failed to execute terraform plan", "workspace", ws.Name)
		_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to execute terraform plan: %v", err), nil)
		return ctrl.Result{}, err, true
	}

	if !changed {
		log.Info("plan has no changes, marking as completed", "workspace", ws.Name)
		now := metav1.Now()
		err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Plan completed - no changes needed", func(s *tfreconcilev1alpha1.WorkspaceStatus) {
			s.HasChanges = false
			s.LastPlanOutput = planOutput
			s.LastExecutionTime = &now
		})
		if err != nil {
			return ctrl.Result{}, err, true
		}
	}

	plan, err := r.createPlanRecord(ctx, *ws, changed, planOutput, "", false, false)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create plan record: %w", err), true

	}

	err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Plan completed", func(s *tfreconcilev1alpha1.WorkspaceStatus) {
		s.HasChanges = changed
		s.NewPlanNeeded = false
		s.LastPlanOutput = planOutput
		s.CurrentPlan = &tfreconcilev1alpha1.PlanReference{
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

func (r *WorkspaceReconciler) handleApply(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	defer log.V(DebugLevel).Info("handle apply completed")
	log.V(DebugLevel).Info("handle apply starting")

	if !ws.Status.NewApplyNeeded {
		return ctrl.Result{}, nil, false
	}

	if !ws.Spec.AutoApply {
		now := metav1.Now()
		r.Recorder.Eventf(ws, v1.EventTypeNormal, TFApplyEventReason, "Auto-apply is disabled, skipping apply")
		err := r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Apply skipped, no Auto-apply enabled", func(s *tfreconcilev1alpha1.WorkspaceStatus) {
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
		applyOutput, err := r.executeTerraformApply(ctx, tf, false)
		if err != nil {
			log.Error(err, "failed to apply terraform", "workspace", ws.Name)
			_ = r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to apply terraform: %v", err), nil)

			return ctrl.Result{}, err, true
		}

		_, err = r.createPlanRecord(ctx, *ws, ws.Status.HasChanges, ws.Status.LastPlanOutput, applyOutput, true, false)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create plan record after failed apply: %w", err), true
		}

		now := metav1.Now()
		err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Apply completed successfully", func(s *tfreconcilev1alpha1.WorkspaceStatus) {
			s.LastExecutionTime = &now
			s.LastApplyOutput = applyOutput
		})

		log.Info("apply completed", "workspace", ws.Name)
		r.Recorder.Eventf(ws, v1.EventTypeNormal, TFApplyEventReason, "Terraform apply completed successfully")
	}

	old := ws.DeepCopy()
	ws.Status.NewApplyNeeded = false
	err := r.Client.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status after apply: %w", err), true
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) setupTerraformForWorkspace(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (ctrl.Result, error, bool, *tfexec.Terraform) {
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

func (r *WorkspaceReconciler) handleDeletionAndFinalizers(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, tf *tfexec.Terraform) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	defer log.V(DebugLevel).Info("handling deletion and finalizers completed")
	log.V(DebugLevel).Info("handling deletion and finalizers starting")

	if ws.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil, false
	}

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

	updated := controllerutil.RemoveFinalizer(ws, workspaceFinalizer)
	if updated {
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err, true
		}
	}

	return ctrl.Result{}, nil, false
}

func (r *WorkspaceReconciler) handleCleanup(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	defer log.V(DebugLevel).Info("cleanup completed")
	log.V(DebugLevel).Info("cleanup starting")
	old := ws.DeepCopy()
	ws.Status.ObservedGeneration = ws.Generation
	ws.Status.NextRefreshTimestamp = metav1.NewTime(time.Now().Add(5 * time.Minute))
	err := r.Client.Status().Patch(ctx, ws, client.MergeFrom(old))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status during cleanup: %w", err), true
	}

	return ctrl.Result{}, nil, false
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Workspace{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}). // Match terraform execution capacity
		Complete(r)
}

func (r *WorkspaceReconciler) executeTerraformPlan(ctx context.Context, tf *tfexec.Terraform, destroy bool) (bool, string, error) {
	var changed bool
	var err error

	changed, err = tf.Plan(ctx, tfexec.Destroy(destroy), tfexec.Out("plan.out"))

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
func (r *WorkspaceReconciler) executeTerraformApply(ctx context.Context, tf *tfexec.Terraform, destroy bool) (string, error) {
	var stdout, stderr bytes.Buffer
	tf.SetStdout(&stdout)
	tf.SetStderr(&stderr)

	var err error
	if destroy {
		err = tf.Destroy(ctx)
	} else {
		err = tf.Apply(ctx)
	}

	return constructOutput(stdout, stderr, err)
}

func constructOutput(stdout, stderr bytes.Buffer, err error) (string, error) {
	var output string
	if stdout.Len() > 0 {
		output += "=== TERRAFORM OUTPUT ===\n"
		output += stdout.String()
	}

	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += "=== TERRAFORM DIAGNOSTICS ===\n"
		output += stderr.String()
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

type workspaceStatusUpdate struct {
	hasChanges     bool
	planOutput     string
	applyOutput    string
	completionTime *metav1.Time
}

// updateWorkspaceStatus updates the terraform execution status in the workspace
func (r *WorkspaceReconciler) updateWorkspaceStatus(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, phase string, message string, updater func(status *tfreconcilev1alpha1.WorkspaceStatus)) error {
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
func (r *WorkspaceReconciler) createPlanRecord(ctx context.Context, ws tfreconcilev1alpha1.Workspace, hasChanges bool, planOutput, applyOutput string, wasApplied bool, destroy bool) (*tfreconcilev1alpha1.Plan, error) {
	planName := fmt.Sprintf("%s-%d", ws.Name, ws.Generation)

	phase := tfreconcilev1alpha1.PlanPhasePlanned
	message := "Plan completed"
	if wasApplied {
		phase = tfreconcilev1alpha1.PlanPhaseApplied
		message = "Plan completed and applied"
	} else if applyOutput != "" {
		// If there's apply output but wasApplied is false, it means apply failed
		phase = tfreconcilev1alpha1.PlanPhaseErrored
		message = "Plan completed but apply failed"
	}
	now := metav1.Now()
	plan := &tfreconcilev1alpha1.Plan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      planName,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				workspacePlanLabel: ws.Name,
			},
		},
		Spec: tfreconcilev1alpha1.PlanSpec{
			WorkspaceRef: tfreconcilev1alpha1.WorkspaceReference{
				Name:      ws.Name,
				Namespace: ws.Namespace,
			},
			AutoApply:        ws.Spec.AutoApply,
			TerraformVersion: ws.Spec.TerraformVersion,
			Render:           ws.Status.CurrentRender,
			Destroy:          destroy,
		},
	}

	err := controllerutil.SetControllerReference(&ws, plan, r.Scheme)
	if err != nil {
		return nil, fmt.Errorf("failed to set controller reference on plan: %w", err)
	}

	err = r.Client.Create(ctx, plan)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create plan audit record: %w", err)
	}

	// The plan status must be updated in a separate call
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
			return err
		}

		// Update the status with the latest outputs
		plan.Status = tfreconcilev1alpha1.PlanStatus{
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
func (r *WorkspaceReconciler) getEnvsForExecution(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (map[string]string, error) {
	if ws.Spec.TFExec == nil {
		return map[string]string{}, nil
	}
	if ws.Spec.TFExec.Env == nil {
		return map[string]string{}, nil
	}
	envs := make(map[string]string)
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
			var secret v1.Secret
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: env.SecretKeyRef.Name}, &secret)
			if err != nil {
				return nil, fmt.Errorf("failed to get secret %s: %w", env.SecretKeyRef.Name, err)
			}
			if val, ok := secret.Data[env.SecretKeyRef.Key]; ok {
				envs[env.Name] = string(val)
				continue
			}
		}
	}

	// Handle AWS authentication with service account tokens
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
	}

	return envs, nil
}

// setupAWSAuthentication creates temporary AWS token file for authentication
func (r *WorkspaceReconciler) setupAWSAuthentication(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (string, error) {
	var sa v1.ServiceAccount
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ws.Namespace,
		Name:      ws.Spec.Authentication.AWS.ServiceAccountName,
	}, &sa)
	if err != nil {
		return "", fmt.Errorf("failed to get service account %s in namespace %s: %w",
			ws.Spec.Authentication.AWS.ServiceAccountName, ws.Namespace, err)
	}

	tokenRequest := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: func(i int64) *int64 { return &i }(600),
		},
	}

	err = r.Client.SubResource("token").Create(ctx, &sa, tokenRequest)
	if err != nil {
		return "", fmt.Errorf("failed to create token for service account %s: %w",
			ws.Spec.Authentication.AWS.ServiceAccountName, err)
	}
	tokenFile, err := os.CreateTemp("", fmt.Sprintf("aws-token-%s-%s-*", ws.Namespace, ws.Name))
	if err != nil {
		return "", fmt.Errorf("failed to create temp token file: %w", err)
	}
	defer tokenFile.Close()

	if _, err := tokenFile.Write([]byte(tokenRequest.Status.Token)); err != nil {
		os.Remove(tokenFile.Name())
		return "", fmt.Errorf("failed to write token to temp file: %w", err)
	}

	return tokenFile.Name(), nil
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
