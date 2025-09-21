package controller

import (
	"context"
	"fmt"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	simplePlanFinalizer = "tf-reconcile.lego.com/plan-finalizer"
)

type SimplePlanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *SimplePlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	if !controllerutil.ContainsFinalizer(&plan, simplePlanFinalizer) {
		controllerutil.AddFinalizer(&plan, simplePlanFinalizer)
		if err := r.Update(ctx, &plan); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

func (r *SimplePlanReconciler) handleDeletion(ctx context.Context, plan *tfreconcilev1alpha1.Plan) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling plan deletion", "plan", plan.Name, "namespace", plan.Namespace)

	if controllerutil.ContainsFinalizer(plan, simplePlanFinalizer) {
		controllerutil.RemoveFinalizer(plan, simplePlanFinalizer)
		if err := r.Update(ctx, plan); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *SimplePlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Plan{}).
		Complete(r)
}
