package controller

import (
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

// PlanReconciler reconciles a Plan object
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

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&plan, planFinalizer) {
		controllerutil.AddFinalizer(&plan, planFinalizer)
		if err := r.Update(ctx, &plan); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Set start time if not set
	if plan.Status.StartTime == nil {
		now := metav1.NewTime(time.Now())
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Client.Get(ctx, req.NamespacedName, &plan); err != nil {
				return err
			}
			plan.Status.StartTime = &now
			plan.Status.Phase = tfreconcilev1alpha1.PlanPhasePlanning
			return r.Client.Status().Update(ctx, &plan)
		})
		if err != nil {
			return ctrl.Result{Requeue: true}, fmt.Errorf("failed to update plan start time: %w", err)
		}
		// Continue execution instead of returning
		plan.Status.Phase = tfreconcilev1alpha1.PlanPhasePlanning
	}

	switch plan.Status.Phase {
	case "", tfreconcilev1alpha1.PlanPhasePending:
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

		// Start planning phase
		result, err := r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhasePlanning, "Starting terraform plan", nil)
		if err != nil {
			return result, err
		}
		// Continue to execute plan immediately
		return r.executePlan(ctx, &plan, *workspace)

	case tfreconcilev1alpha1.PlanPhasePlanning:
		// Check if this plan is still eligible to run before continuing
		if !plan.Spec.Destroy {
			canRun, err := r.canPlanRun(ctx, &plan)
			if err != nil {
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseErrored,
					fmt.Sprintf("Failed to check plan eligibility: %v", err), nil)
			}
			if !canRun {
				log.Info("plan is no longer eligible to run (newer plan exists)", "plan", plan.Name)
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseCancelled,
					"Plan cancelled during planning - newer plan exists for workspace", nil)
			}
		}

		return r.executePlan(ctx, &plan, *workspace)

	case tfreconcilev1alpha1.PlanPhasePlanned:
		if plan.Spec.AutoApply {
			// If there are no changes and this is not a destroy plan, mark as applied (nothing to do)
			if !plan.Status.HasChanges && !plan.Spec.Destroy {
				log.Info("plan has no changes, marking as applied", "plan", plan.Name)
				now := metav1.NewTime(time.Now())
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseApplied, "Plan completed - no changes needed", &planStatusUpdate{
					completionTime: &now,
				})
			}

			// For plans with changes or destroy plans, proceed with apply
			if plan.Status.HasChanges || plan.Spec.Destroy {
				// For destroy plans, proceed even if HasChanges is false -
				// this could mean there's nothing to destroy, which is a successful outcome

				// Destroy plans should always be allowed to proceed regardless of newer plans
				if !plan.Spec.Destroy {
					// Check if this plan is still eligible to auto-apply (only for non-destroy plans)
					canApply, err := r.canPlanAutoApply(ctx, &plan)
					if err != nil {
						return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseErrored,
							fmt.Sprintf("Failed to check plan eligibility: %v", err), nil)
					}
					if !canApply {
						log.Info("plan is no longer eligible for auto-apply (newer plan exists)", "plan", plan.Name)
						return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseCancelled,
							"Plan cancelled - newer plan exists for workspace", nil)
					}
				}

				// Start applying phase
				result, err := r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseApplying, "Starting terraform apply", nil)
				if err != nil {
					return result, err
				}
				// Continue to execute apply immediately
				return r.executeApply(ctx, &plan, *workspace)
			}
		}
		return ctrl.Result{}, nil

	case tfreconcilev1alpha1.PlanPhaseApplying:
		// Double-check that this plan is still eligible before applying (only for non-destroy plans)
		if plan.Spec.AutoApply && !plan.Spec.Destroy {
			canApply, err := r.canPlanAutoApply(ctx, &plan)
			if err != nil {
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseErrored,
					fmt.Sprintf("Failed to check plan eligibility: %v", err), nil)
			}
			if !canApply {
				log.Info("plan is no longer eligible for auto-apply during apply phase (newer plan exists)", "plan", plan.Name)
				return r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhaseCancelled,
					"Plan cancelled during apply - newer plan exists for workspace", nil)
			}
		}
		return r.executeApply(ctx, &plan, *workspace)

	case tfreconcilev1alpha1.PlanPhaseCancelled:
		// Plan was cancelled, no further action needed
		// Do not restart cancelled plans even if they have auto-apply enabled
		log.Info("plan is cancelled, no further action needed", "plan", plan.Name)
		return ctrl.Result{}, nil

	case tfreconcilev1alpha1.PlanPhaseApplied:
		return ctrl.Result{}, nil

	case tfreconcilev1alpha1.PlanPhaseErrored:
		// For errored plans, reset to planning phase to retry
		// This allows plans to retry when underlying issues (like expired tokens) are fixed
		log.Info("retrying errored plan", "plan", plan.Name, "previousError", plan.Status.Message)
		result, err := r.updatePlanStatus(ctx, &plan, tfreconcilev1alpha1.PlanPhasePlanning, "Retrying after previous error", nil)
		if err != nil {
			return result, err
		}
		// Continue to execute plan immediately
		return r.executePlan(ctx, &plan, *workspace)

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

func (r *PlanReconciler) executePlan(ctx context.Context, plan *tfreconcilev1alpha1.Plan, workspace tfreconcilev1alpha1.Workspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get environment variables for execution
	envs, err := r.getEnvsForExecution(ctx, workspace)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get envs for execution: %v", err), nil)
	}

	// Clean up temporary AWS token file at the end
	if tempTokenPath, exists := envs["AWS_WEB_IDENTITY_TOKEN_FILE"]; exists {
		defer func() {
			if err := os.Remove(tempTokenPath); err != nil {
				log.Error(err, "failed to cleanup temp token file", "file", tempTokenPath)
			}
		}()
	}

	// Get terraform executable
	tf, terraformRCPath, err := r.Tf.GetTerraformForWorkspace(ctx, workspace)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get terraform executable: %v", err), nil)
	}

	// Set up environment
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

	// Write the HCL content to main.tf first, before terraform init
	// For destroy plans, we still need the original HCL content for proper state management
	err = r.writeHclContent(tf.WorkingDir(), plan.Spec.Render)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to write HCL content: %v", err), nil)
	}

	// Initialize terraform
	err = r.Tf.TerraformInit(ctx, tf)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to init terraform: %v", err), nil)
	}

	// Validate terraform
	valResult, err := tf.Validate(ctx)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to validate terraform: %v", err), nil)
	}

	r.Recorder.Eventf(plan, v1.EventTypeNormal, PlanTFValidateEventReason, "Terraform validation completed, valid: %t", valResult.Valid)

	// Execute plan
	var changed bool
	if plan.Spec.Destroy {
		// For destroy plans, use terraform plan -destroy
		changed, err = tf.Plan(ctx, tfexec.Destroy(true), tfexec.Out("plan.out"))
	} else {
		// For normal plans
		changed, err = tf.Plan(ctx, tfexec.Out("plan.out"))
	}
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to plan terraform: %v", err), nil)
	}

	// Get plan output
	planOutput, err := tf.ShowPlanFileRaw(ctx, "plan.out")
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to show plan file: %v", err), nil)
	}

	r.Recorder.Eventf(plan, v1.EventTypeNormal, PlanTFPlanEventReason, "Terraform plan completed, changes: %t", changed)

	// Update plan status with results
	result, err := r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhasePlanned, "Plan completed successfully", &planStatusUpdate{
		hasChanges:  changed,
		validRender: valResult.Valid,
		planOutput:  planOutput,
	})
	if err != nil {
		return result, err
	}

	// If auto-apply is enabled and there are changes, continue to apply phase
	if plan.Spec.AutoApply && changed {
		// Update status to applying
		result, err := r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseApplying, "Starting terraform apply", nil)
		if err != nil {
			return result, err
		}
		// Continue to execute apply immediately
		return r.executeApply(ctx, plan, workspace)
	}

	return ctrl.Result{}, nil
}

func (r *PlanReconciler) executeApply(ctx context.Context, plan *tfreconcilev1alpha1.Plan, workspace tfreconcilev1alpha1.Workspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get environment variables for execution
	envs, err := r.getEnvsForExecution(ctx, workspace)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get envs for execution: %v", err), nil)
	}

	// Clean up temporary AWS token file at the end
	if tempTokenPath, exists := envs["AWS_WEB_IDENTITY_TOKEN_FILE"]; exists {
		defer func() {
			if err := os.Remove(tempTokenPath); err != nil {
				log.Error(err, "failed to cleanup temp token file", "file", tempTokenPath)
			}
		}()
	}

	// Get terraform executable
	tf, terraformRCPath, err := r.Tf.GetTerraformForWorkspace(ctx, workspace)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to get terraform executable: %v", err), nil)
	}

	// Set up environment
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

	// Write the HCL content to main.tf
	// For destroy plans, we still need the original HCL content for proper state management
	err = r.writeHclContent(tf.WorkingDir(), plan.Spec.Render)
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to write HCL content: %v", err), nil)
	}

	// For more reliable execution, use direct apply without saved plan file
	// This prevents "stale plan" errors when state changes between plan and apply phases
	if plan.Spec.Destroy {
		// For destroy plans, run destroy with auto-approve
		err = tf.Apply(ctx, tfexec.Destroy(true))
	} else {
		// For normal plans, run apply with auto-approve
		err = tf.Apply(ctx)
	}
	if err != nil {
		return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseErrored,
			fmt.Sprintf("Failed to apply terraform: %v", err), nil)
	}

	r.Recorder.Eventf(plan, v1.EventTypeNormal, PlanTFApplyEventReason, "Terraform apply completed successfully")

	// Update plan status to applied
	now := metav1.NewTime(time.Now())
	return r.updatePlanStatus(ctx, plan, tfreconcilev1alpha1.PlanPhaseApplied, "Apply completed successfully", &planStatusUpdate{
		completionTime: &now,
	})
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
// For destroy plans, they can always run.
// For regular plans, only the latest plan for a workspace can run.
func (r *PlanReconciler) canPlanRun(ctx context.Context, plan *tfreconcilev1alpha1.Plan) (bool, error) {
	// Destroy plans can always run
	if plan.Spec.Destroy {
		return true, nil
	}

	// Get all plans for this workspace (excluding destroy plans)
	planList := &tfreconcilev1alpha1.PlanList{}
	err := r.Client.List(ctx, planList,
		client.InNamespace(plan.Namespace),
		client.MatchingLabels{"tf-reconcile.lego.com/workspace": plan.Spec.WorkspaceRef.Name},
	)
	if err != nil {
		return false, fmt.Errorf("failed to list plans for workspace: %w", err)
	}

	// Find the newest non-destroy plan for this workspace
	var newestPlan *tfreconcilev1alpha1.Plan
	for i := range planList.Items {
		p := &planList.Items[i]
		// Skip destroy plans for this comparison
		if p.Spec.Destroy {
			continue
		}
		if newestPlan == nil || p.CreationTimestamp.After(newestPlan.CreationTimestamp.Time) {
			newestPlan = p
		}
	}

	// If no plans found (shouldn't happen), allow this one
	if newestPlan == nil {
		return true, nil
	}

	// This plan can run only if it's the newest plan
	return newestPlan.Name == plan.Name, nil
}

// canPlanAutoApply checks if this plan is eligible for auto-apply.
// It ensures only the latest plan for a workspace can auto-apply, unless:
// - The latest plan doesn't have autoApply=true AND
// - This plan has autoApply=true
func (r *PlanReconciler) canPlanAutoApply(ctx context.Context, plan *tfreconcilev1alpha1.Plan) (bool, error) {
	// First check if the plan can run at all
	canRun, err := r.canPlanRun(ctx, plan)
	if err != nil {
		return false, err
	}
	if !canRun {
		return false, nil
	}

	// Get all plans for this workspace
	planList := &tfreconcilev1alpha1.PlanList{}
	err = r.Client.List(ctx, planList,
		client.InNamespace(plan.Namespace),
		client.MatchingLabels{"tf-reconcile.lego.com/workspace": plan.Spec.WorkspaceRef.Name},
	)
	if err != nil {
		return false, fmt.Errorf("failed to list plans for workspace: %w", err)
	}

	// Find the newest plan for this workspace (excluding destroy plans for this logic)
	var newestPlan *tfreconcilev1alpha1.Plan
	for i := range planList.Items {
		p := &planList.Items[i]
		// Skip destroy plans for this comparison
		if p.Spec.Destroy {
			continue
		}
		if newestPlan == nil || p.CreationTimestamp.After(newestPlan.CreationTimestamp.Time) {
			newestPlan = p
		}
	}

	// If no plans found (shouldn't happen), allow this one
	if newestPlan == nil {
		return true, nil
	}

	// If this plan is the newest plan, it can auto-apply
	if newestPlan.Name == plan.Name {
		return true, nil
	}

	// If there's a newer plan, check if the newer plan has autoApply disabled
	// In that case, allow the current plan to auto-apply if it has autoApply enabled
	if !newestPlan.Spec.AutoApply && plan.Spec.AutoApply {
		return true, nil
	}

	// Otherwise, this plan should not auto-apply
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Plan{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Complete(r)
}
