package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/LEGO/kube-tf-reconciler/pkg/render"
	"github.com/LEGO/kube-tf-reconciler/pkg/runner"
	"github.com/hashicorp/hcl/v2/hclwrite"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	TFErrEventReason     = "TerraformError"
	TFPlanEventReason    = "TerraformPlan"
	TFApplyEventReason   = "TerraformApply"
	TFDestroyEventReason = "TerraformDestroy"

	workspaceFinalizer = "tf-reconcile.lego.com/finalizer"
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

	}

	// Handle deletion first
	if !ws.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
			return r.handleWorkspaceDeletion(ctx, &ws)
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

	err := r.cancelAndDeleteOlderPlans(ctx, ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to cancel older plans: %w", err)
	}

	needsNewPlan, err := r.needsNewPlan(ctx, ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check if new plan is needed: %w", err)
	}

	if !needsNewPlan && r.alreadyProcessedOnce(ws) {
		log.Info("already processed workspace, skipping")
		return ctrl.Result{}, nil
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

	if needsNewPlan {
		plan, err := r.createOrUpdatePlan(ctx, ws, string(result))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create or update plan: %w", err)
		}

		// Update workspace status with current plan reference
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
				return err
			}
			ws.Status.CurrentPlan = &tfreconcilev1alpha1.PlanReference{
				Name:      plan.Name,
				Namespace: plan.Namespace,
			}
			ws.Status.ObservedGeneration = ws.Generation
			return r.Client.Status().Update(ctx, &ws)
		})
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
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

func (r *WorkspaceReconciler) createOrUpdatePlan(ctx context.Context, ws tfreconcilev1alpha1.Workspace, render string) (*tfreconcilev1alpha1.Plan, error) {
	planName := fmt.Sprintf("%s-%d", ws.Name, ws.Generation)

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
		},
	}

	err := r.Client.Create(ctx, plan)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create plan: %w", err)
	}

	if apierrors.IsAlreadyExists(err) {
		existingPlan := &tfreconcilev1alpha1.Plan{}
		err = r.Client.Get(ctx, client.ObjectKey{Name: planName, Namespace: ws.Namespace}, existingPlan)
		if err != nil {
			return nil, fmt.Errorf("failed to get existing plan: %w", err)
		}
		return existingPlan, nil
	}

	return plan, nil
}

func (r *WorkspaceReconciler) handleWorkspaceDeletion(ctx context.Context, ws *tfreconcilev1alpha1.Workspace) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Step 1: If preventDestroy is true, skip terraform destroy and go straight to cleanup
	if ws.Spec.PreventDestroy {
		log.Info("skipping terraform destroy due to the preventDestroy flag")
		r.Recorder.Eventf(ws, v1.EventTypeNormal, "DestroyPrevented",
			"Terraform destroy skipped due to preventDestroy")
		return r.cleanupWorkspaceAndFinalize(ctx, ws)
	}

	// Step 1.5: Check if destroy has already been completed (to prevent creating multiple destroy plans)
	if ws.Annotations != nil && ws.Annotations["tf-reconcile.lego.com/destroy-completed"] == "true" {
		log.Info("destroy already completed, proceeding to cleanup")
		return r.cleanupWorkspaceAndFinalize(ctx, ws)
	}

	// Step 2: Check if we have a destroy plan in progress
	destroyPlan, err := r.getDestroyPlan(ctx, *ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check for destroy plan: %w", err)
	}

	// Step 3: If no destroy plan exists, create one
	if destroyPlan == nil {
		err := r.createDestroyPlan(ctx, *ws)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create destroy plan: %w", err)
		}

		log.Info("created destroy plan for workspace deletion")
		r.Recorder.Eventf(ws, v1.EventTypeNormal, TFDestroyEventReason, "Created destroy plan for workspace deletion")

		return ctrl.Result{RequeueAfter: time.Second * 20}, nil
	}

	// Step 4: Check destroy plan status
	if destroyPlan.Status.Phase == tfreconcilev1alpha1.PlanPhaseApplied {
		log.Info("destroy plan completed successfully")
		r.Recorder.Eventf(ws, v1.EventTypeNormal, TFDestroyEventReason, "Successfully destroyed resources")

		// Mark destroy as completed to prevent race conditions
		err := r.markDestroyCompleted(ctx, ws)
		if err != nil {
			log.Error(err, "failed to mark destroy as completed, but proceeding with cleanup")
		}

		return r.cleanupWorkspaceAndFinalize(ctx, ws)

	} else {
		log.Info("waiting for destroy plan to complete", "phase", destroyPlan.Status.Phase)
		return ctrl.Result{RequeueAfter: time.Second * 20}, nil
	}
}

func (r *WorkspaceReconciler) getDestroyPlan(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (*tfreconcilev1alpha1.Plan, error) {
	planList := &tfreconcilev1alpha1.PlanList{}
	err := r.Client.List(ctx, planList,
		client.InNamespace(ws.Namespace),
		client.MatchingLabels{
			"tf-reconcile.lego.com/workspace": ws.Name,
			"tf-reconcile.lego.com/destroy":   "true",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list destroy plans for workspace: %w", err)
	}

	var latestDestroyPlan *tfreconcilev1alpha1.Plan
	for i := range planList.Items {
		plan := &planList.Items[i]
		if latestDestroyPlan == nil || plan.CreationTimestamp.After(latestDestroyPlan.CreationTimestamp.Time) {
			latestDestroyPlan = plan
		}
	}

	return latestDestroyPlan, nil
}

func (r *WorkspaceReconciler) createDestroyPlan(ctx context.Context, ws tfreconcilev1alpha1.Workspace) error {
	// Use the current workspace render state for the destroy plan
	// This ensures we destroy based on the most recent workspace configuration
	render, err := r.renderHclForWorkspace(ws)
	if err != nil {
		return fmt.Errorf("failed to render workspace for destroy plan: %w", err)
	}

	destroyPlanName := fmt.Sprintf("%s-destroy-%d", ws.Name, time.Now().Unix())

	destroyPlan := &tfreconcilev1alpha1.Plan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      destroyPlanName,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				"tf-reconcile.lego.com/workspace": ws.Name,
				"tf-reconcile.lego.com/destroy":   "true",
			},
			// Don't set owner reference for destroy plans so they can complete
			// even after the workspace is deleted
		},
		Spec: tfreconcilev1alpha1.PlanSpec{
			WorkspaceRef: tfreconcilev1alpha1.WorkspaceReference{
				Name:      ws.Name,
				Namespace: ws.Namespace,
			},
			AutoApply:        true,
			TerraformVersion: ws.Spec.TerraformVersion,
			Render:           string(render),
			Destroy:          true,
		},
	}

	return r.Client.Create(ctx, destroyPlan)
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

	// First pass: Mark all plans for deletion if they're not already being deleted
	plansToDelete := len(planList.Items) > 0
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

	// If we just marked plans for deletion, requeue to wait for them to be actually deleted
	if plansToDelete {
		log.Info("waiting for plans to be deleted before finalizing workspace")
		return ctrl.Result{RequeueAfter: time.Second * 20}, nil
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
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}

func (r *WorkspaceReconciler) alreadyProcessedOnce(ws tfreconcilev1alpha1.Workspace) bool {
	// A workspace is considered processed if:
	// 1. ObservedGeneration matches Generation (resource hasn't changed)
	// 2. AND it has been actually processed (has CurrentRender set)
	// This prevents skipping newly created workspaces that haven't been processed yet
	return ws.Status.ObservedGeneration == ws.Generation && ws.Status.CurrentRender != ""
}

func (r *WorkspaceReconciler) dueForRefresh(t time.Time, ws tfreconcilev1alpha1.Workspace) bool {
	return t.After(ws.Status.NextRefreshTimestamp.Time)
}

func (r *WorkspaceReconciler) cancelAndDeleteOlderPlans(ctx context.Context, ws tfreconcilev1alpha1.Workspace) error {
	log := logf.FromContext(ctx)

	planList := &tfreconcilev1alpha1.PlanList{}
	err := r.Client.List(ctx, planList,
		client.InNamespace(ws.Namespace),
		client.MatchingLabels{"tf-reconcile.lego.com/workspace": ws.Name},
	)
	if err != nil {
		return fmt.Errorf("failed to list plans for workspace: %w", err)
	}

	currentPlanName := fmt.Sprintf("%s-%d", ws.Name, ws.Generation)

	//Cancel all older running or errored plans first
	for i := range planList.Items {
		plan := &planList.Items[i]

		if plan.Name == currentPlanName || plan.Spec.Destroy {
			continue
		}

		// Skip if already in a truly terminal state (applied or cancelled)
		// Note: Errored plans are NOT skipped because they retry automatically
		if plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseApplied ||
			plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseCancelled {
			continue
		}

		// Cancel the plan if it's running or errored
		if plan.Status.Phase == tfreconcilev1alpha1.PlanPhasePlanning ||
			plan.Status.Phase == tfreconcilev1alpha1.PlanPhasePlanned ||
			plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseErrored {

			log.Info("cancelling older plan", "plan", plan.Name, "currentPhase", plan.Status.Phase)

			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Client.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
					return err
				}
				plan.Status.Phase = tfreconcilev1alpha1.PlanPhaseCancelled
				plan.Status.Message = "Plan cancelled - newer plan exists for workspace"
				plan.Status.ObservedGeneration = plan.Generation // Mark as processed to prevent restart
				return r.Client.Status().Update(ctx, plan)
			})
			if err != nil {
				log.Error(err, "failed to cancel older plan", "plan", plan.Name)
			}
		}
	}

	// If there are more than 3 plans total, delete the oldest ones
	if len(planList.Items) > 3 {

		// Sort plans by creation timestamp (oldest first)
		sort.Slice(planList.Items, func(i, j int) bool {
			return planList.Items[i].CreationTimestamp.Time.Before(planList.Items[j].CreationTimestamp.Time)
		})

		// Calculate how many plans to delete (keep only 3)
		plansToDelete := len(planList.Items) - 3

		for i := 0; i < plansToDelete; i++ {
			plan := &planList.Items[i]

			if plan.Name == currentPlanName || plan.Spec.Destroy {
				continue
			}

			if plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseApplied ||
				plan.Status.Phase == tfreconcilev1alpha1.PlanPhaseCancelled {

				log.Info("deleting old plan to maintain limit of 3 plans", "plan", plan.Name, "phase", plan.Status.Phase)

				err := r.Client.Delete(ctx, plan)
				if err != nil {
					log.Error(err, "failed to delete old plan", "plan", plan.Name)
				}
			}
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
