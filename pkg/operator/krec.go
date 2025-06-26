package operator

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type KrecReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	NamespaceLabel string
}

func (k *KrecReconciler) SetupWithManager(mgr ctrl.Manager) error {
	k.Recorder = mgr.GetEventRecorderFor("krec")
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Complete(k)
}

func (k *KrecReconciler) Reconcile(ctx context.Context, request ctrl.Request) (reconcile.Result, error) {
	slog.Info("received reconcile request", "request", request)
	return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
}
