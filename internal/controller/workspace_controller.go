package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-exec/tfexec"
	authv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
	"lukaspj.io/kube-tf-reconciler/pkg/render"
	"lukaspj.io/kube-tf-reconciler/pkg/runner"
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

	// Finalizer name
	workspaceFinalizer = "tf-reconcile.lukaspj.io/finalizer"
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

	if r.alreadyProcessedOnce(ws) {
		log.Info("already processed workspace, skipping")
		return ctrl.Result{}, nil
	}

	envs, err := r.getEnvsForExecution(ctx, ws)
	if err != nil {
		err = fmt.Errorf("failed to get envs for execution: %w", err)
		r.Recorder.Eventf(&ws, v1.EventTypeWarning, TFErrEventReason, err.Error())
		return ctrl.Result{}, err
	}

	// Clean up temporary token file at the end of reconciliation
	if tempTokenPath, exists := envs["AWS_WEB_IDENTITY_TOKEN_FILE"]; exists {
		defer func() {
			if err := os.Remove(tempTokenPath); err != nil {
				log.Error(err, "failed to cleanup temp token file", "file", tempTokenPath)
			}
		}()
	}

	tf, terraformRCPath, err := r.Tf.GetTerraformForWorkspace(ctx, ws)
	if err != nil {
		err = fmt.Errorf("failed to get terraform executable %s: %w", req.String(), err)
		r.Recorder.Eventf(&ws, v1.EventTypeWarning, TFErrEventReason, err.Error())
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
		err = fmt.Errorf("failed to set terraform env: %w", err)
		r.Recorder.Eventf(&ws, v1.EventTypeWarning, TFErrEventReason, err.Error())
		return ctrl.Result{}, err
	}

	result, err := r.renderHcl(tf.WorkingDir(), ws)
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

	err = r.Tf.TerraformInit(ctx, tf)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init workspace: %w", err)
	}

	valResult, err := tf.Validate(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to validate workspace: %w", err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
			return err
		}
		ws.Status.ValidRender = valResult.Valid
		return r.Client.Status().Update(ctx, &ws)
	})

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status %s: %w", req.String(), err)
	}

	if !ws.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
			err = tf.Destroy(ctx)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to destroy resource: %w", err)
			}

			r.Recorder.Eventf(&ws, v1.EventTypeNormal, TFDestroyEventReason, "Successfully destroyed resources")

			controllerutil.RemoveFinalizer(&ws, workspaceFinalizer)
			if err := r.Update(ctx, &ws); err != nil {
				return ctrl.Result{}, err
			}
			ws.Status.ObservedGeneration = ws.Generation
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
					return err
				}
				ws.Status.ValidRender = valResult.Valid
				return r.Client.Status().Update(ctx, &ws)
			})

			return ctrl.Result{}, err

		}
		// Stop reconciliation as resource is being deleted
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
		controllerutil.AddFinalizer(&ws, workspaceFinalizer)
		if err := r.Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	changed, err := tf.Plan(ctx, tfexec.Out("plan.out"))
	if err != nil {
		err = fmt.Errorf("failed to plan workspace %s: %w", req.String(), err)
		r.Recorder.Eventf(&ws, v1.EventTypeWarning, TFErrEventReason, err.Error())
		return ctrl.Result{}, err
	}
	plan, err := tf.ShowPlanFileRaw(ctx, "plan.out")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to show plan file: %w", err)
	}
	r.Recorder.Eventf(&ws, v1.EventTypeNormal, TFPlanEventReason, "Workspace %s planned", req.String())
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
			return err
		}
		ws.Status.LatestPlan = plan
		return r.Client.Status().Update(ctx, &ws)
	})

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update workspace status %s: %w", req.String(), err)
	}

	log.WithValues("changed", changed).Info("planned workspace")

	if ws.Spec.AutoApply && changed {
		err = tf.Apply(ctx)
		if err != nil {
			err = fmt.Errorf("failed to apply workspace %s: %w", req.String(), err)
			r.Recorder.Eventf(&ws, v1.EventTypeWarning, TFErrEventReason, err.Error())
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&ws, v1.EventTypeNormal, TFApplyEventReason, "Workspace %s applied", req.String())
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
			return err
		}
		ws.Status.ObservedGeneration = ws.Generation
		return r.Client.Status().Update(ctx, &ws)
	})
	return ctrl.Result{}, err
}

func (r *WorkspaceReconciler) renderHcl(workspaceDir string, ws tfreconcilev1alpha1.Workspace) ([]byte, error) {
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

	err = os.WriteFile(filepath.Join(workspaceDir, "main.tf"), f.Bytes(), 0644)
	if err != nil {
		return f.Bytes(), fmt.Errorf("%w: failed to write workspace: %w", renderErr, err)
	}

	return f.Bytes(), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfreconcilev1alpha1.Workspace{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}

func (r *WorkspaceReconciler) alreadyProcessedOnce(ws tfreconcilev1alpha1.Workspace) bool {
	return ws.Status.ObservedGeneration == ws.Generation
}

func (r *WorkspaceReconciler) dueForRefresh(t time.Time, ws tfreconcilev1alpha1.Workspace) bool {
	return t.After(ws.Status.NextRefreshTimestamp.Time)
}

func (r *WorkspaceReconciler) refreshState(ctx context.Context, ws tfreconcilev1alpha1.Workspace) error {
	_, _, err := r.Tf.GetTerraformForWorkspace(ctx, ws)
	if err != nil {
		return fmt.Errorf("refreshState: failed to get terraform executable %s: %w", ws.Name, err)
	}

	ws.Status.NextRefreshTimestamp = metav1.NewTime(time.Now().Add(time.Minute * 5))
	return r.Client.Status().Update(ctx, &ws)
}

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
