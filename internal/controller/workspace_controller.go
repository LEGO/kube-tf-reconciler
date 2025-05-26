/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	WorkerStartedAnnotation = "tf-reconcile.lukaspj.io/worker-started"
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=tf-reconcile.lukaspj.io,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tf-reconcile.lukaspj.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tf-reconcile.lukaspj.io,resources=workspaces/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Workspace object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var ws tfreconcilev1alpha1.Workspace
	if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get workspace %s: %w", req.String(), err)
		}

		return ctrl.Result{}, nil
	}

	hcl := `terraform {
}
`

	cfg := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workspace-config",
			Namespace: ws.Namespace,
		},
		Data: map[string]string{
			"workspace": hcl,
		},
	}

	res, err := controllerutil.CreateOrPatch(ctx, r.Client, &cfg, func() error {
		cfg.Data["workspace"] = hcl
		return nil
	})
	log.WithValues("configmap_result", res).Info("set configmap")

	// Check if the worker pod already exists
	pod, err := r.startWorkerPod(ctx, ws)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to start worker pod: %w", err)
	}
	log.WithValues("pod", pod.Status.Phase).Info("start worker pod")

	logs, err := r.receiveLogs(ctx, pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to receive logs: %w", err)
	}

	log.Info("pod executed successfully", "logs", logs)

	err = r.deleteWorkerPod(ctx, pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete worker pod: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Workspace{}).
		Named("workspace").
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Owns(&v1.Pod{}, builder.MatchEveryOwner).
		Complete(r)
}

func (r *WorkspaceReconciler) startWorkerPod(ctx context.Context, ws tfreconcilev1alpha1.Workspace) (*v1.Pod, error) {
	podName := fmt.Sprintf("worker-%s", ws.Name)
	namespace := ws.Namespace

	// Define the pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: *ws.Spec.WorkerSpec,
	}

	if err := controllerutil.SetOwnerReference(&ws, pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference: %w", err)
	}

	// Create the pod
	if err := r.Client.Create(ctx, pod); err != nil {
		return nil, fmt.Errorf("failed to create pod: %w", err)
	}

	// Wait for the pod to become ready
	err := r.waitForPodToBeRunning(ctx, pod)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for pod to be: %w", err)
	}

	return pod, nil
}

func (r *WorkspaceReconciler) waitForPodToBeRunning(ctx context.Context, pod *v1.Pod) error {
	timeout := time.After(2 * time.Minute) // Set a timeout for waiting
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timed out waiting for pod %s to be running", pod.Name)
		case <-ticker.C:
			// Fetch the latest pod status
			currentPod := &v1.Pod{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, currentPod); err != nil {
				return err
			}

			// Check the pod status
			if currentPod.Status.Phase == v1.PodRunning || currentPod.Status.Phase == v1.PodSucceeded {
				return nil
			} else if currentPod.Status.Phase == v1.PodFailed {
				return fmt.Errorf("pod %s is in phase %s", pod.Name, currentPod.Status.Phase)
			}
		}
	}
}

func (r *WorkspaceReconciler) deleteWorkerPod(ctx context.Context, pod *v1.Pod) error {
	err := r.Delete(ctx, pod)
	if err != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}

	return nil
}

// receiveLogsOld from the pod
func (r *WorkspaceReconciler) receiveLogsOld(ctx context.Context, pod *v1.Pod) ([]string, error) {
	config, err := rest.InClusterConfig()
	var logs []string
	if err != nil {
		return logs, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return logs, fmt.Errorf("failed to create clientset: %w", err)
	}

	// Stream the pod logs
	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &v1.PodLogOptions{Follow: true})
	stream, err := req.Stream(ctx)
	if err != nil {
		return logs, fmt.Errorf("failed to stream logs: %w", err)
	}
	defer stream.Close()

	reader := bufio.NewReader(stream)
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return logs, fmt.Errorf("error reading log stream: %w", err)
		}
		logs = append(logs, line)
	}

	return logs, nil
}

func (r *WorkspaceReconciler) receiveLogs(ctx context.Context, pod *v1.Pod) ([]string, error) {

	c := kubernetes.NewForConfigOrDie(ctrl.GetConfigOrDie())
	var logs []string
	// Stream the pod logs
	req := c.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &v1.PodLogOptions{Follow: true})
	stream, err := req.Stream(ctx)
	if err != nil {
		return logs, fmt.Errorf("failed to stream logs: %w", err)
	}
	defer stream.Close()

	reader := bufio.NewReader(stream)
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return logs, fmt.Errorf("error reading log stream: %w", err)
		}
		logs = append(logs, line)
	}

	return logs, nil
}
