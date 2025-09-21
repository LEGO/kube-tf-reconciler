package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/hashicorp/hcl/v2/hclwrite"
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
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Tf *runner.Exec
}

func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	reqStart := time.Now()
	var ws tfreconcilev1alpha1.Workspace
	if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get workspace %s: %w", req.String(), err)
		}
		return ctrl.Result{}, nil
	}

	if r.dueForRefresh(reqStart, ws) {
		// TODO: Implement refresh logic if needed
	}

	// Handle deletion first
	if !ws.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
			return handleWorkspaceDeletion(ctx, r, req, ws)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
		controllerutil.AddFinalizer(&ws, workspaceFinalizer)
		if err := r.Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	needsNewPlan, err := r.needsNewPlan(ctx, ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check if new plan is needed: %w", err)
	}

	if !needsNewPlan || r.alreadyProcessedOnce(ws) {
		log.Info("already processed workspace, skipping")
		return ctrl.Result{}, nil
	}

	return planAndApplyWorkspace(ctx, r, req, ws, false)
}

func planAndApplyWorkspace(ctx context.Context, r *WorkspaceReconciler, req ctrl.Request, ws tfreconcilev1alpha1.Workspace, destroy bool) (ctrl.Result, error) {
	err := r.cleanupOldPlans(ctx, ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cleanup old plans: %w", err)
	}

	result, err := r.renderHclForWorkspace(ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to render workspace %s: %w", req.String(), err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
			return err
		}
		ws.Status.CurrentRender = string(result)
		return r.Client.Status().Update(ctx, &ws)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status %s: %w", req.String(), err)
	}

	return r.executeTerraform(ctx, &ws, string(result), destroy)
}

func (r *WorkspaceReconciler) renderHclForWorkspace(ws tfreconcilev1alpha1.Workspace) ([]byte, error) {
	f := hclwrite.NewEmptyFile()
	err := render.Workspace(f.Body(), ws)
	renderErr := fmt.Errorf("failed to render workspace %s/%s", ws.Namespace, ws.Name)
	if err != nil {
		return f.Bytes(), fmt.Errorf("%w: %w", renderErr, err)
	}

	err = render.Providers(f.Body(), ws.Spec.ProviderSpecs)
	if err != nil {
		return f.Bytes(), fmt.Errorf("%w: failed to render providers: %w", renderErr, err)
	}

	err = render.Module(f.Body(), ws.Spec.Module)
	if err != nil {
		return f.Bytes(), fmt.Errorf("%w: failed to render module: %w", renderErr, err)
	}

	return f.Bytes(), nil
}

func handleWorkspaceDeletion(ctx context.Context, r *WorkspaceReconciler, req ctrl.Request, ws tfreconcilev1alpha1.Workspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if ws.Spec.PreventDestroy {
		log.Info("skipping terraform destroy due to the preventDestroy flag")
		r.Recorder.Eventf(&ws, v1.EventTypeNormal, "DestroyPrevented",
			"Terraform destroy skipped due to preventDestroy")
		return r.cleanupWorkspaceAndFinalize(ctx, &ws)
	}

	// check if destroy has already been completed
	if ws.Annotations != nil && ws.Annotations["tf-reconcile.lego.com/destroy-completed"] == "true" {
		log.Info("destroy already completed, proceeding to cleanup")
		return r.cleanupWorkspaceAndFinalize(ctx, &ws)
	}

	return planAndApplyWorkspace(ctx, r, req, ws, true)
}

func (r *WorkspaceReconciler) cleanupWorkspaceAndFinalize(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	planList := &tfreconcilev1alpha1.PlanList{}
	err := r.Client.List(ctx, planList,
		client.InNamespace(ws.Namespace),
		client.MatchingLabels{"tf-reconcile.lego.com/workspace": ws.Name},
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list plans for cleanup: %w", err)
	}

	for i := range planList.Items {
		plan := &planList.Items[i]
		if plan.DeletionTimestamp.IsZero() { // Only delete if not already being deleted
			err := r.Client.Delete(ctx, plan)
			if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete plan %s: %w", plan.Name, err)
			}
			log.Info("marked plan for deletion", "plan", plan.Name)
		}
	}

	err = r.Tf.CleanupWorkspace(*ws)
	if err != nil {
		log.Error(err, "failed to cleanup workspace directory, but proceeding with finalization")
		r.Recorder.Eventf(ws, v1.EventTypeWarning, "CleanupFailed",
			"Failed to cleanup workspace directory: %v", err)
	}

	controllerutil.RemoveFinalizer(ws, workspaceFinalizer)
	if err := r.Update(ctx, ws); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("successfully finalized workspace deletion - all plans have been deleted and directory cleaned up")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Workspace{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}). // Match terraform execution capacity
		Complete(r)
}

func (r *WorkspaceReconciler) alreadyProcessedOnce(ws tfreconcilev1alpha1.Workspace) bool {
	if ws.Status.ObservedGeneration != ws.Generation || ws.Status.CurrentRender == "" {
		return false
	}

	// Check if the current render would be the same
	newRender, err := r.renderHclForWorkspace(ws)
	if err != nil {
		return false
	}

	return ws.Status.CurrentRender == string(newRender)
}
func (r *WorkspaceReconciler) dueForRefresh(t time.Time, ws tfreconcilev1alpha1.Workspace) bool {
	return t.After(ws.Status.NextRefreshTimestamp.Time)
}

func (r *WorkspaceReconciler) cleanupOldPlans(ctx context.Context, ws tfreconcilev1alpha1.Workspace) error {
	log := logf.FromContext(ctx)

	planList := &tfreconcilev1alpha1.PlanList{}
	err := r.Client.List(ctx, planList,
		client.InNamespace(ws.Namespace),
		client.MatchingLabels{"tf-reconcile.lego.com/workspace": ws.Name},
	)
	if err != nil {
		return fmt.Errorf("failed to list plans for workspace: %w", err)
	}

	// Clean up old completed plans (keep only 3 most recent)
	if len(planList.Items) <= 3 {
		return nil
	}

	sort.Slice(planList.Items, func(i, j int) bool {
		return planList.Items[i].CreationTimestamp.Time.Before(planList.Items[j].CreationTimestamp.Time)
	})

	currentPlanName := fmt.Sprintf("%s-%d", ws.Name, ws.Generation)
	plansToDelete := len(planList.Items) - 3

	for i := 0; i < plansToDelete; i++ {
		plan := &planList.Items[i]

		if plan.Name == currentPlanName || plan.Spec.Destroy {
			continue
		}

		log.Info("deleting old plan audit record", "plan", plan.Name, "phase", plan.Status.Phase)

		err := r.Client.Delete(ctx, plan)
		if err != nil {
			log.Error(err, "failed to delete old plan audit record", "plan", plan.Name)
		}
	}

	return nil
}

func (r *WorkspaceReconciler) needsNewPlan(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (bool, error) {
	expectedPlanName := fmt.Sprintf("%s-%d", ws.Name, ws.Generation)

	var currentPlan tfreconcilev1alpha1.Plan
	err := r.Client.Get(ctx, client.ObjectKey{
		Name:      expectedPlanName,
		Namespace: ws.Namespace,
	}, &currentPlan)

	if apierrors.IsNotFound(err) {
		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("failed to get current plan: %w", err)
	}

	// Plan exists, check if workspace has changed since plan was created
	if ws.Status.ObservedGeneration != ws.Generation {
		return true, nil
	}

	if ws.Status.CurrentPlan == nil || ws.Status.CurrentPlan.Name != expectedPlanName {
		return true, nil
	}

	return false, nil
}

func (r *WorkspaceReconciler) markDestroyCompleted(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) error {
	// Add annotation to mark destroy as completed
	if ws.Annotations == nil {
		ws.Annotations = make(map[string]string)
	}
	ws.Annotations["tf-reconcile.lego.com/destroy-completed"] = "true"

	return r.Update(ctx, ws)
}

// executes terraform plan/apply inline and creates a Plan CRD as audit record
func (r *WorkspaceReconciler) executeTerraform(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, render string, destroy bool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	err := r.updateWorkspaceStatus(ctx, ws, TFPhasePlanning, "Starting terraform plan", nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status: %w", err)
	}

	envs, err := r.getEnvsForExecution(ctx, *ws)
	if err != nil {
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to get envs for execution: %v", err), nil)
		return ctrl.Result{}, err
	}

	cleanup := func() {
		if tempTokenPath, exists := envs["AWS_WEB_IDENTITY_TOKEN_FILE"]; exists {
			if err := os.Remove(tempTokenPath); err != nil {
				log.Error(err, "failed to cleanup temp token file", "file", tempTokenPath)
			}
		}
	}
	defer cleanup()

	tf, terraformRCPath, err := r.Tf.GetTerraformForWorkspace(ctx, *ws)
	if err != nil {
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to get terraform executable: %v", err), nil)
		return ctrl.Result{}, err
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
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to set terraform env: %v", err), nil)
		return ctrl.Result{}, err
	}

	err = r.writeHclContent(tf.WorkingDir(), render)
	if err != nil {
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to write HCL content: %v", err), nil)
		return ctrl.Result{}, err
	}

	err = r.Tf.TerraformInit(ctx, tf)
	if err != nil {
		log.Error(err, "failed to init terraform", "workspace", ws.Name)
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to init terraform: %v", err), nil)
		return ctrl.Result{}, err
	}

	valResult, err := tf.Validate(ctx)
	if err != nil {
		log.Error(err, "failed to validate terraform", "workspace", ws.Name)
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to validate terraform: %v", err), nil)
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(ws, v1.EventTypeNormal, TFValidateEventReason, "Terraform validation completed, valid: %t", valResult.Valid)

	if !valResult.Valid {
		log.Error(err, "Terraform validation failed", "workspace", ws.Name)
		diagnosticsMsg := r.formatValidationDiagnostics(valResult.Diagnostics)
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, diagnosticsMsg, nil)
		return ctrl.Result{}, fmt.Errorf("terraform validation failed: %s", diagnosticsMsg)
	}

	changed, planOutput, err := r.executeTerraformPlan(ctx, tf, destroy)
	if err != nil {
		log.Error(err, "failed to execute terraform plan", "workspace", ws.Name)
		r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to execute terraform plan: %v", err), nil)
		return ctrl.Result{}, err
	}

	log.Info("terraform plan completed", "workspace", ws.Name, "changes", changed)
	r.Recorder.Eventf(ws, v1.EventTypeNormal, TFPlanEventReason, "Terraform plan completed, changes: %t", changed)

	var applyOutput string
	if ws.Spec.AutoApply {
		if !changed {
			log.Info("plan has no changes, marking as completed", "workspace", ws.Name)
			now := metav1.NewTime(time.Now())
			err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Plan completed - no changes needed", &workspaceStatusUpdate{
				hasChanges:     false,
				planOutput:     planOutput,
				completionTime: &now,
			})
			if err != nil {
				return ctrl.Result{}, err
			}
		} else {
			err = r.updateWorkspaceStatus(ctx, ws, TFPhaseApplying, "Applying terraform changes", nil)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update workspace status: %w", err)
			}

			applyOutput, err = r.executeTerraformApply(ctx, tf, destroy)
			if err != nil {
				log.Error(err, "failed to apply terraform", "workspace", ws.Name)
				r.updateWorkspaceStatus(ctx, ws, TFPhaseErrored, fmt.Sprintf("Failed to apply terraform: %v", err), &workspaceStatusUpdate{
					hasChanges:  changed,
					planOutput:  planOutput,
					applyOutput: applyOutput,
				})

				plan, planErr := r.createPlanRecord(ctx, *ws, render, changed, planOutput, applyOutput, false, destroy)
				if planErr != nil {
					log.Error(planErr, "failed to create plan audit record for failed apply", "workspace", ws.Name)
				} else if plan != nil {
					retry.RetryOnConflict(retry.DefaultRetry, func() error {
						if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ws), ws); err != nil {
							return err
						}
						ws.Status.CurrentPlan = &tfreconcilev1alpha1.PlanReference{
							Name:      plan.Name,
							Namespace: plan.Namespace,
						}
						return r.Client.Status().Update(ctx, ws)
					})
				}

				return ctrl.Result{}, err
			}

			log.Info("terraform apply completed successfully", "workspace", ws.Name)
			r.Recorder.Eventf(ws, v1.EventTypeNormal, TFApplyEventReason, "Terraform apply completed successfully")

			now := metav1.NewTime(time.Now())
			err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Apply completed successfully", &workspaceStatusUpdate{
				hasChanges:     changed,
				planOutput:     planOutput,
				applyOutput:    applyOutput,
				completionTime: &now,
			})
			if destroy == true {
				// If this was a destroy operation, mark destroy as completed
				err = r.markDestroyCompleted(ctx, ws)
			}

			if err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// Plan completed but no auto-apply
		now := metav1.NewTime(time.Now())
		err = r.updateWorkspaceStatus(ctx, ws, TFPhaseCompleted, "Plan completed successfully", &workspaceStatusUpdate{
			hasChanges:     changed,
			planOutput:     planOutput,
			completionTime: &now,
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Create Plan as audit record after successful execution
	plan, err := r.createPlanRecord(ctx, *ws, render, changed, planOutput, applyOutput, ws.Spec.AutoApply && changed, destroy)
	if err != nil {
		log.Error(err, "failed to create plan audit record", "workspace", ws.Name)
		// Don't fail the reconciliation if we can't create the audit record
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ws), ws); err != nil {
			return err
		}
		if plan != nil {
			ws.Status.CurrentPlan = &tfreconcilev1alpha1.PlanReference{
				Name:      plan.Name,
				Namespace: plan.Namespace,
			}
		}
		ws.Status.ObservedGeneration = ws.Generation
		return r.Client.Status().Update(ctx, ws)
	})

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace observed generation: %w", err)
	}

	return ctrl.Result{}, nil
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

// executeTerraformApply executes the terraform apply or destroy command
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

// writeHclContent writes HCL content to the terraform working directory
func (r *WorkspaceReconciler) writeHclContent(workspaceDir, content string) error {
	return os.WriteFile(filepath.Join(workspaceDir, "main.tf"), []byte(content), 0644)
}

type workspaceStatusUpdate struct {
	hasChanges     bool
	planOutput     string
	applyOutput    string
	completionTime *metav1.Time
}

// updateWorkspaceStatus updates the terraform execution status in the workspace
func (r *WorkspaceReconciler) updateWorkspaceStatus(ctx context.Context, ws *tfreconcilev1alpha1.Workspace, phase string, message string, update *workspaceStatusUpdate) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ws), ws); err != nil {
			return err
		}

		ws.Status.TerraformPhase = phase
		ws.Status.TerraformMessage = message

		if update != nil {
			if update.completionTime != nil {
				ws.Status.LastExecutionTime = update.completionTime
			}
			ws.Status.HasChanges = update.hasChanges
			if update.planOutput != "" {
				ws.Status.LastPlanOutput = update.planOutput
			}
			if update.applyOutput != "" {
				ws.Status.LastApplyOutput = update.applyOutput
			}
		}

		return r.Client.Status().Update(ctx, ws)
	})
	return err
}

// createPlanRecord creates a Plan CRD as an audit record after terraform execution
func (r *WorkspaceReconciler) createPlanRecord(ctx context.Context, ws tfreconcilev1alpha1.Workspace, render string, hasChanges bool, planOutput, applyOutput string, wasApplied bool, destroy bool) (*tfreconcilev1alpha1.Plan, error) {
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

	now := metav1.NewTime(time.Now())
	plan := &tfreconcilev1alpha1.Plan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      planName,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				"tf-reconcile.lego.com/workspace": ws.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: ws.APIVersion,
					Kind:       ws.Kind,
					Name:       ws.Name,
					UID:        ws.UID,
					Controller: func(b bool) *bool { return &b }(true),
				},
			},
		},
		Spec: tfreconcilev1alpha1.PlanSpec{
			WorkspaceRef: tfreconcilev1alpha1.WorkspaceReference{
				Name:      ws.Name,
				Namespace: ws.Namespace,
			},
			AutoApply:        ws.Spec.AutoApply,
			TerraformVersion: ws.Spec.TerraformVersion,
			Render:           render,
			Destroy:          destroy,
		},
	}

	err := r.Client.Create(ctx, plan)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create plan audit record: %w", err)
	}

	// The plan status must be updated in a separate call
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, client.ObjectKey{Name: planName, Namespace: ws.Namespace}, plan); err != nil {
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
func (r *WorkspaceReconciler) getEnvsForExecution(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (map[string]string, error) {
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
func (r *WorkspaceReconciler) setupAWSAuthentication(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (string, error) {
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
	diagnosticsMsg := "Terraform validation failed with the following diagnostics:\n\n"
	for i, diag := range diagnostics {
		diagnosticsMsg += fmt.Sprintf("Diagnostic %d:\n", i+1)
		diagnosticsMsg += fmt.Sprintf("  Severity: %s\n", diag.Severity)
		diagnosticsMsg += fmt.Sprintf("  Summary: %s\n", diag.Summary)

		if diag.Detail != "" {
			diagnosticsMsg += fmt.Sprintf("  Detail: %s\n", diag.Detail)
		}

		if diag.Range != nil {
			diagnosticsMsg += fmt.Sprintf("  Location: %s:%d:%d\n",
				diag.Range.Filename, diag.Range.Start.Line, diag.Range.Start.Column)
		}

		if diag.Snippet != nil {
			if diag.Snippet.Context != nil {
				diagnosticsMsg += fmt.Sprintf("  Context: %s\n", *diag.Snippet.Context)
			}
			if diag.Snippet.Code != "" {
				diagnosticsMsg += fmt.Sprintf("  Code: %s\n", diag.Snippet.Code)
			}
		}

		diagnosticsMsg += "\n"
	}
	return diagnosticsMsg
}
