package ui

import (
	"context"
	"fmt"
	"sort"
	"time"

	v1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// internalScheme is used when building new clients for context-switching.
var internalScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))
	return s
}()

// listKubeContexts returns all context names from the default kubeconfig and
// the name of the current context.
func listKubeContexts() ([]string, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := loadingRules.Load()
	if err != nil {
		return nil, "", fmt.Errorf("loading kubeconfig: %w", err)
	}
	names := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, config.CurrentContext, nil
}

// listNamespaces returns all namespace names visible to the client.
func listNamespaces(ctx context.Context, k8sClient client.Client) ([]string, error) {
	var nsList corev1.NamespaceList
	if err := k8sClient.List(ctx, &nsList); err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	names := make([]string, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		names = append(names, ns.Name)
	}
	sort.Strings(names)
	return names, nil
}

type WorkspaceSummary struct {
	Name                 string `json:"name"`
	Namespace            string `json:"namespace"`
	TerraformPhase       string `json:"terraformPhase"`
	TerraformMessage     string `json:"terraformMessage"`
	HasChanges           bool   `json:"hasChanges"`
	NewPlanNeeded        bool   `json:"newPlanNeeded"`
	NewApplyNeeded       bool   `json:"newApplyNeeded"`
	CreationTimestamp    string `json:"creationTimestamp"`
	NextRefreshTimestamp string `json:"nextRefreshTimestamp"`
	LastErrorMessage     string `json:"lastErrorMessage"`
}

type WorkspaceDetail struct {
	WorkspaceSummary
	TerraformVersion  string       `json:"terraformVersion"`
	AutoApply         bool         `json:"autoApply"`
	Destroy           string       `json:"destroy"`
	LastPlanOutput    string       `json:"lastPlanOutput"`
	LastApplyOutput   string       `json:"lastApplyOutput"`
	CurrentRender     string       `json:"currentRender"`
	InitOutput        string       `json:"initOutput"`
	RetryCount        int32        `json:"retryCount"`
	LastErrorTime     string       `json:"lastErrorTime"`
	LastExecutionTime string       `json:"lastExecutionTime"`
	Plan              *PlanSummary `json:"plan,omitempty"`
}

type PlanSummary struct {
	Name           string `json:"name"`
	Phase          string `json:"phase"`
	HasChanges     bool   `json:"hasChanges"`
	StartTime      string `json:"startTime,omitempty"`
	CompletionTime string `json:"completionTime,omitempty"`
	PlanOutput     string `json:"planOutput,omitempty"`
	ApplyOutput    string `json:"applyOutput,omitempty"`
	Message        string `json:"message,omitempty"`
}

func timeStr(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func metaTimeStr(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func fetchWorkspaces(ctx context.Context, k8sClient client.Client, namespace string) ([]WorkspaceSummary, error) {
	var list v1alpha1.WorkspaceList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := k8sClient.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}

	summaries := make([]WorkspaceSummary, 0, len(list.Items))
	for _, ws := range list.Items {
		summaries = append(summaries, WorkspaceSummary{
			Name:                 ws.Name,
			Namespace:            ws.Namespace,
			TerraformPhase:       ws.Status.TerraformPhase,
			TerraformMessage:     ws.Status.TerraformMessage,
			HasChanges:           ws.Status.HasChanges,
			NewPlanNeeded:        ws.Status.NewPlanNeeded,
			NewApplyNeeded:       ws.Status.NewApplyNeeded,
			CreationTimestamp:    metaTimeStr(ws.CreationTimestamp),
			NextRefreshTimestamp: metaTimeStr(ws.Status.NextRefreshTimestamp),
			LastErrorMessage:     ws.Status.LastErrorMessage,
		})
	}
	return summaries, nil
}

// listWorkspacePlans returns all Plan objects for a workspace, newest first.
func listWorkspacePlans(ctx context.Context, k8sClient client.Client, namespace, workspaceName string) ([]PlanSummary, error) {
	var planList v1alpha1.PlanList
	if err := k8sClient.List(ctx, &planList,
		client.InNamespace(namespace),
		client.MatchingLabels{v1alpha1.WorkspacePlanLabel: workspaceName},
	); err != nil {
		return nil, fmt.Errorf("listing plans for %s/%s: %w", namespace, workspaceName, err)
	}
	// Sort newest creation timestamp first.
	sort.Slice(planList.Items, func(i, j int) bool {
		return planList.Items[i].CreationTimestamp.After(planList.Items[j].CreationTimestamp.Time)
	})
	summaries := make([]PlanSummary, 0, len(planList.Items))
	for _, p := range planList.Items {
		summaries = append(summaries, PlanSummary{
			Name:           p.Name,
			Phase:          string(p.Status.Phase),
			HasChanges:     p.Status.HasChanges,
			StartTime:      timeStr(p.Status.StartTime),
			CompletionTime: timeStr(p.Status.CompletionTime),
			PlanOutput:     p.Status.PlanOutput,
			ApplyOutput:    p.Status.ApplyOutput,
			Message:        p.Status.Message,
		})
	}
	return summaries, nil
}

func fetchWorkspaceDetail(ctx context.Context, k8sClient client.Client, namespace, name string) (*WorkspaceDetail, error) {
	var ws v1alpha1.Workspace
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &ws); err != nil {
		return nil, fmt.Errorf("getting workspace %s/%s: %w", namespace, name, err)
	}

	detail := &WorkspaceDetail{
		WorkspaceSummary: WorkspaceSummary{
			Name:                 ws.Name,
			Namespace:            ws.Namespace,
			TerraformPhase:       ws.Status.TerraformPhase,
			TerraformMessage:     ws.Status.TerraformMessage,
			HasChanges:           ws.Status.HasChanges,
			NewPlanNeeded:        ws.Status.NewPlanNeeded,
			NewApplyNeeded:       ws.Status.NewApplyNeeded,
			CreationTimestamp:    metaTimeStr(ws.CreationTimestamp),
			NextRefreshTimestamp: metaTimeStr(ws.Status.NextRefreshTimestamp),
			LastErrorMessage:     ws.Status.LastErrorMessage,
		},
		TerraformVersion:  ws.Spec.TerraformVersion,
		AutoApply:         ws.Spec.AutoApply,
		Destroy:           string(ws.Spec.Destroy),
		LastPlanOutput:    ws.Status.LastPlanOutput,
		LastApplyOutput:   ws.Status.LastApplyOutput,
		CurrentRender:     ws.Status.CurrentRender,
		InitOutput:        ws.Status.InitOutput,
		RetryCount:        ws.Status.Backoff.RetryCount,
		LastErrorTime:     timeStr(ws.Status.LastErrorTime),
		LastExecutionTime: timeStr(ws.Status.LastExecutionTime),
	}

	if ws.Status.CurrentPlan != nil {
		planNS := ws.Status.CurrentPlan.Namespace
		if planNS == "" {
			planNS = ws.Namespace
		}
		var plan v1alpha1.Plan
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: planNS, Name: ws.Status.CurrentPlan.Name}, &plan)
		if err == nil {
			detail.Plan = &PlanSummary{
				Name:           plan.Name,
				Phase:          string(plan.Status.Phase),
				HasChanges:     plan.Status.HasChanges,
				StartTime:      timeStr(plan.Status.StartTime),
				CompletionTime: timeStr(plan.Status.CompletionTime),
				PlanOutput:     plan.Status.PlanOutput,
				ApplyOutput:    plan.Status.ApplyOutput,
				Message:        plan.Status.Message,
			}
		}
	}

	return detail, nil
}
