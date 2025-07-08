package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/hashicorp/terraform-exec/tfexec"
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
	PlanTFErrEventReason      = "PlanTerraformError"
	PlanTFPlanEventReason     = "PlanTerraformPlan"
	PlanTFApplyEventReason    = "PlanTerraformApply"
	PlanTFValidateEventReason = "PlanTerraformValidate"

	planFinalizer = "tf-reconcile.lego.com/plan-finalizer"
)

type PlanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Tf *runner.Exec
}

func (r *PlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var plan tfreconcilev1alpha1.Plan
	if err := r.Client.Get(ctx, req.NamespacedName, &plan); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get plan %s: %w", req.String(), err)
		}
		return ctrl.Result{}, nil
	}

	if !plan.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &plan)
	}

	// Skip if already processed, but first check for orphaned destroy plans
	if r.alreadyProcessed(plan) {
		// For destroy plans that are completed, check if their workspace still exists
		// If not, delete the orphaned destroy plan
		if plan.Spec.Destroy && plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseApplied {
			_, err := r.getWorkspace(ctx, plan)
			if apierrors.IsNotFound(err) {
				log.Info("destroy plan already completed and workspace is gone, deleting orphaned plan", "plan", plan.Name)
				err := r.Client.Delete(ctx, &plan)
				if err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to delete orphaned destroy plan: %w", err)
				}
				return ctrl.Result{}, nil
			}
		}

		log.Info("plan already processed, skipping")
		return ctrl.Result{}, nil
	}

	workspace, err := r.getWorkspace(ctx, plan)
	if err != nil {
		// For destroy plans, if the workspace is not found, it means it was already deleted
		// In this case, we can safely mark the destroy plan as completed since there's nothing to destroy
		if apierrors.IsNotFound(err) && plan.Spec.Destroy {
			log.Info("workspace not found for destroy plan, marking as completed", "plan", plan.Name)

			return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseApplied,
				"Destroy plan completed - workspace already deleted", &planStatusUpdate{
					completionTime: func() *metav1.Time { now := metav1.NewTime(time.Now()); return &now }(),
				})
		}
		return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get workspace: %v", err), nil)
	}

	if !controllerutil.ContainsFinalizer(&plan, planFinalizer) {
		controllerutil.AddFinalizer(&plan, planFinalizer)
		if err := r.Update(ctx, &plan); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if plan.Status.StartTime == nil {
		now := metav1.NewTime(time.Now())
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Client.Get(ctx, req.NamespacedName, &plan); err != nil {
				return err
			}
			plan.Status.StartTime = &now
			plan.Status.Phase = tfreconcilev1alpha1.PlanPhasePlanning
			plan.Status.ObservedGeneration = plan.Generation
			return r.Client.Status().Update(ctx, &plan)
		})
		if err != nil {
			return ctrl.Result{Requeue: true}, fmt.Errorf("failed to update plan start time: %w", err)
		}

		plan.Status.Phase = tfreconcilev1alpha1.PlanPhasePlanning
	}

	switch plan.Status.Phase {
	case "", tfreconcilev1alpha1.PlanPhasePlanning:
		// Check if this plan is still eligible to run before starting
		if !plan.Spec.Destroy {
			canRun, err := r.canPlanRun(ctx, &plan)
			if err != nil {
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseErrored,
					fmt.Sprintf("Failed to check plan eligibility: %v", err), nil)
			}
			if !canRun {
				log.Info("plan is not eligible to run (newer plan exists)", "plan", plan.Name)
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseCancelled,
					"Plan cancelled - newer plan exists for workspace", nil)
			}
		}

		return r.executePlanWorkflow(ctx, &plan, *workspace)

	// TODO this phase might not be needed anymore, since we handle AutoApply above
	case tfreconcilev1alpha1.PlanPhasePlanned:
		if plan.Spec.AutoApply {
			log.Info("plan is in planned state but has auto-apply enabled, re-executing workflow")
			return r.executePlanWorkflow(ctx, &plan, *workspace)
		}
		return ctrl.Result{}, nil

	case tfreconcilev1alpha1.PlanPhaseCancelled:
		log.Info("plan is cancelled, no further action needed", "plan", plan.Name)
		return ctrl.Result{}, nil

	case tfreconcilev1alpha1.PlanPhaseApplied:
		return ctrl.Result{}, nil

	case tfreconcilev1alpha1.PlanPhaseErrored:
		log.Info("retrying errored plan", "plan", plan.Name, "previousError", plan.Status.Message)
		return r.executePlanWorkflow(ctx, &plan, *workspace)

	default:
		log.Info("unknown plan phase", "phase", plan.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *PlanReconciler) getWorkspace(ctx context.Context, plan tfreconcilev1alpha1.Plan) (*tfreconcilev1alpha1.Workspace, error) {
	namespace := plan.Spec.WorkspaceRef.Namespace
	if namespace == "" {
		namespace = plan.Namespace
	}

	var workspace tfreconcilev1alpha1.Workspace
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      plan.Spec.WorkspaceRef.Name,
		Namespace: namespace,
	}, &workspace)
	if err != nil {
		return nil, fmt.Errorf("failed to get workspace %s/%s: %w", namespace, plan.Spec.WorkspaceRef.Name, err)
	}

	return &workspace, nil
}

// executePlanWorkflow handles the complete plan workflow (plan and potentially apply) in one go
func (r *PlanReconciler) executePlanWorkflow(ctx context.Context, plan *tfreconcilev1alpha1.Plan, workspace tfreconcilev1alpha1.Workspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Set up terraform environment (shared between plan and apply)
	envs, err := r.getEnvsForExecution(ctx, workspace)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get envs for execution: %v", err), nil)
	}

	cleanup := func() {
		if tempTokenPath, exists := envs["AWS_WEB_IDENTITY_TOKEN_FILE"]; exists {
			if err := os.Remove(tempTokenPath); err != nil {
				log.Error(err, "failed to cleanup temp token file", "file", tempTokenPath)
			}
		}
	}
	defer cleanup()

	// Get terraform executable
	tf, terraformRCPath, err := r.Tf.GetTerraformForWorkspace(ctx, workspace)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get terraform executable: %v", err), nil)
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
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to set terraform env: %v", err), nil)
	}

	err = r.writeHclContent(tf.WorkingDir(), plan.Spec.Render)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to write HCL content: %v", err), nil)
	}

	err = r.Tf.TerraformInit(ctx, tf)
	if err != nil {
		log.Error(err, "failed to init terraform", "plan", plan.Name)
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to init terraform: %v", err), nil)
	}

	valResult, err := tf.Validate(ctx)
	if err != nil {
		log.Error(err, "failed to validate terraform", "plan", plan.Name)
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to validate terraform: %v", err), nil)
	}

	r.Recorder.Eventf(plan, v1.EventTypeNormal, PlanTFValidateEventReason, "Terraform validation completed, valid: %t", valResult.Valid)

	if !valResult.Valid {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			"Terraform validation failed", nil)
	}

	changed, planOutput, err := r.executeTerraformPlan(ctx, tf, plan)
	if err != nil {
		log.Error(err, "failed to execute terraform plan", "plan", plan.Name)
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to execute terraform plan: %v", err), nil)
	}

	log.Info("terraform plan completed", "plan", plan.Name, "changes", changed)
	r.Recorder.Eventf(plan, v1.EventTypeNormal, PlanTFPlanEventReason, "Terraform plan completed, changes: %t", changed)

	if plan.Spec.AutoApply {

		if !changed && !plan.Spec.Destroy {
			log.Info("plan has no changes, marking as applied", "plan", plan.Name)
			now := metav1.NewTime(time.Now())
			return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseApplied, "Plan completed - no changes needed", &planStatusUpdate{
				hasChanges:     false,
				validRender:    true,
				planOutput:     planOutput,
				completionTime: &now,
			})
		}

		if changed || plan.Spec.Destroy {
			if !plan.Spec.Destroy {
				canApply, err := r.canPlanRun(ctx, plan)
				if err != nil {
					return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
						fmt.Sprintf("Failed to check plan eligibility: %v", err), nil)
				}
				if !canApply {
					log.Info("plan is no longer eligible for auto-apply (newer plan exists)", "plan", plan.Name)
					return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseCancelled,
						"Plan cancelled - newer plan exists for workspace", nil)
				}
			}

			// For destroy plans, proceed even if changed is false -
			// this could mean there's nothing to destroy, which is a successful outcome
			applyOutput, err := r.executeTerraformApply(ctx, tf, plan)
			if err != nil {
				log.Error(err, "failed to apply terraform", "plan", plan.Name)
				return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
					fmt.Sprintf("Failed to apply terraform: %v", err), nil)
			}

			log.Info("terraform apply completed successfully", "plan", plan.Name)
			r.Recorder.Eventf(plan, v1.EventTypeNormal, PlanTFApplyEventReason, "Terraform apply completed successfully")

			// Update plan status to applied
			now := metav1.NewTime(time.Now())
			return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseApplied, "Apply completed successfully", &planStatusUpdate{
				hasChanges:     changed,
				validRender:    true,
				planOutput:     planOutput,
				applyOutput:    applyOutput,
				completionTime: &now,
			})
		}
	}

	// Plan completed but no auto-apply - update status to Planned with plan output
	return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhasePlanned, "Plan completed successfully", &planStatusUpdate{
		hasChanges:  changed,
		validRender: true,
		planOutput:  planOutput,
	})
}

// executeTerraformPlan executes the terraform plan command
func (r *PlanReconciler) executeTerraformPlan(ctx context.Context, tf *tfexec.Terraform, plan *tfreconcilev1alpha1.Plan) (bool, string, error) {
	// Execute plan
	var changed bool
	var err error

	if plan.Spec.Destroy {
		// For destroy plans, use terraform plan -destroy
		changed, err = tf.Plan(ctx, tfexec.Destroy(true), tfexec.Out("plan.out"))
	} else {
		// For normal plans
		changed, err = tf.Plan(ctx, tfexec.Out("plan.out"))
	}
	if err != nil {
		return false, "", fmt.Errorf("failed to plan terraform: %w", err)
	}

	// Get plan output
	planOutput, err := tf.ShowPlanFileRaw(ctx, "plan.out")
	if err != nil {
		return false, "", fmt.Errorf("failed to show plan file: %w", err)
	}

	return changed, planOutput, nil
}

// executeTerraformApply executes the terraform apply command
func (r *PlanReconciler) executeTerraformApply(ctx context.Context, tf *tfexec.Terraform, plan *tfreconcilev1alpha1.Plan) (string, error) {
	// Create buffers to capture stdout and stderr
	var stdout, stderr bytes.Buffer
	tf.SetStdout(&stdout)
	tf.SetStderr(&stderr)

	var err error
	if plan.Spec.Destroy {
		err = tf.Destroy(ctx)
	} else {
		err = tf.Apply(ctx)
	}

	// Build comprehensive output
	output := ""

	// Add stdout content
	if stdout.Len() > 0 {
		output += "=== TERRAFORM OUTPUT ===\n"
		output += stdout.String()
	}

	// Add stderr content (warnings, errors, diagnostics)
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += "=== TERRAFORM DIAGNOSTICS ===\n"
		output += stderr.String()
	}

	// If there was an error, add error details
	if err != nil && output != "" {
		output += "\n=== ERROR DETAILS ===\n"
		output += fmt.Sprintf("Exit error: %v", err)
	}

	// If no output was captured but there was an error, at least show the error
	if output == "" && err != nil {
		output = fmt.Sprintf("Terraform command failed: %v", err)
	}

	return output, err
}

func (r *PlanReconciler) handleDeletion(ctx context.Context, plan *tfreconcilev1alpha1.Plan) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(plan, planFinalizer) {
		controllerutil.RemoveFinalizer(plan, planFinalizer)
		if err := r.Update(ctx, plan); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *PlanReconciler) writeHclContent(workspaceDir, content string) error {
	return os.WriteFile(filepath.Join(workspaceDir, "main.tf"), []byte(content), 0644)
}

type planStatusUpdate struct {
	hasChanges     bool
	validRender    bool
	planOutput     string
	applyOutput    string
	completionTime *metav1.Time
}

func (r *PlanReconciler) updatePlanStatus(ctx context.Context, plan *tfreconcilev1alpha1.Plan, phase tfreconcilev1alpha1.PlanPhase, message string, update *planStatusUpdate) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
			return err
		}

		plan.Status.Phase = phase
		plan.Status.Message = message
		plan.Status.ObservedGeneration = plan.Generation

		if update != nil {
			if update.completionTime != nil {
				plan.Status.CompletionTime = update.completionTime
			}
			plan.Status.HasChanges = update.hasChanges
			plan.Status.ValidRender = update.validRender
			if update.planOutput != "" {
				plan.Status.PlanOutput = update.planOutput
			}
			if update.applyOutput != "" {
				plan.Status.ApplyOutput = update.applyOutput
			}
		}

		return r.Client.Status().Update(ctx, plan)
	})
	return ctrl.Result{}, err
}

func (r *PlanReconciler) alreadyProcessed(plan tfreconcilev1alpha1.Plan) bool {
	return plan.Status.ObservedGeneration == plan.Generation &&
		(plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseApplied ||
			plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseCancelled ||
			(plan.Status.Phase == tfreconcilev1alpha1.PlanPhasePlanned && !plan.Spec.AutoApply))
}

// getEnvsForExecution is similar to the workspace controller version but works with workspace reference
func (r *PlanReconciler) getEnvsForExecution(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (map[string]string, error) {
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

func (r *PlanReconciler) setupAWSAuthentication(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (string, error) {
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

// canPlanRun checks if this plan is eligible to run.
// Rules:
// 1. Only one type of plan (regular or destroy) can run per workspace at a time
// 2. Destroy plans take priority over regular plans
// 3. Among plans of the same type, only the newest can run
func (r *PlanReconciler) canPlanRun(ctx context.Context, plan *tfreconcilev1alpha1.Plan) (bool, error) {
	// Get all plans for this workspace
	planList := &tfreconcilev1alpha1.PlanList{}
	err := r.Client.List(ctx, planList,
		client.InNamespace(plan.Namespace),
		client.MatchingLabels{"tf-reconcile.lego.com/workspace": plan.Spec.WorkspaceRef.Name},
	)
	if err != nil {
		return false, fmt.Errorf("failed to list plans for workspace: %w", err)
	}

	// Separate plans by type and find the newest of each
	var newestRegularPlan *tfreconcilev1alpha1.Plan
	var newestDestroyPlan *tfreconcilev1alpha1.Plan

	for i := range planList.Items {
		p := &planList.Items[i]
		if p.Spec.Destroy {
			if newestDestroyPlan == nil || p.CreationTimestamp.After(newestDestroyPlan.CreationTimestamp.Time) {
				newestDestroyPlan = p
			}
		} else {
			if newestRegularPlan == nil || p.CreationTimestamp.After(newestRegularPlan.CreationTimestamp.Time) {
				newestRegularPlan = p
			}
		}
	}

	// If this is a destroy plan
	if plan.Spec.Destroy {
		// Only the newest destroy plan can run
		if newestDestroyPlan == nil || newestDestroyPlan.Name != plan.Name {
			return false, nil
		}
		// Destroy plans take priority - they can run even if there are regular plans
		return true, nil
	}

	// If this is a regular plan
	// First check if there are any destroy plans - if so, regular plans cannot run
	if newestDestroyPlan != nil {
		return false, nil
	}

	// If no destroy plans, only the newest regular plan can run
	if newestRegularPlan == nil || newestRegularPlan.Name != plan.Name {
		return false, nil
	}

	return true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Plan{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Complete(r)
}
