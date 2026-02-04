package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DestroyBehaviour string

// These are valid destroy behaviours. "DestroyBehaviourSkip" means nothing is
// done when a Workspace is deleted. "DestroyBehaviourAuto" means that a
// `terraform destroy` action is automatically invoked when the resource is
// deleted and "DestroyBehaviourManual" means that the Workspace will not be
// deleted until a destroy action has been invoked manually via the
// ManualDestroyAnnotation.
const (
	DestroyBehaviourSkip   DestroyBehaviour = "skip"
	DestroyBehaviourAuto   DestroyBehaviour = "auto"
	DestroyBehaviourManual DestroyBehaviour = "manual"
)

const (
	ManualApplyAnnotation   = "tf-reconcile.lego.com/manual-apply"
	ManualDestroyAnnotation = "tf-reconcile.lego.com/manual-destroy"
	WorkspacePlanLabel      = "tf-reconcile.lego.com/workspace"
	WorkspaceFinalizer      = "tf-reconcile.lego.com/finalizer"
)

// BackendSpec defines the backend configuration for the workspace
type BackendSpec struct {
	// Type is the type of the backend
	// +kubebuilder:validation:Enum=local;remote;s3;gcs;azurerm;oss;consul;cos;http;pg;kubernetes
	Type string `json:"type"`
	// Inputs are the inputs to the terraform module.
	Inputs *apiextensionsv1.JSON `json:"inputs,omitempty"`
}

type ModuleOutput struct {
	// Name is the name of the output
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EnvVar represents an environment variable present in the terraform process.
type EnvVar struct {
	// Name of the environment variable.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Value of the environment variable.
	// either Value or ConfigMapKeyRef or SecretKeyRef must be set
	// +kubebuilder:validation:Optional
	Value string `json:"value,omitempty"`
	// Selects a key of a ConfigMap.
	// +kubebuilder:validation:Optional
	ConfigMapKeyRef *ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
	// Selects a key of a secret in the Workspace namespace
	// +kubebuilder:validation:Optional
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// ConfigMapKeySelector Selects a key from a ConfigMap.
type ConfigMapKeySelector struct {
	// The Name of the ConfigMap in the Workspace namespace to select from.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// The Key to select.
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// SecretKeySelector selects a key of a Secret.
type SecretKeySelector struct {
	// The Name of the secret in the Workspace namespace to select from.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// The Key of the secret to select from. Must be a valid secret key.
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// ProviderSpec defines the desired state of Provider.
type ProviderSpec struct {
	// Name is the name of the provider.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Source is the source of the provider.
	Source string `json:"source"`
	// Version is the version of the provider.
	// +kubebuilder:validation:Optional
	Version string `json:"version,omitempty"`
}

// ModuleSpec defines the desired state of Module.
type ModuleSpec struct {
	// Name is the name of the terraform module.
	// Example:
	// name: "my-module"
	// source:  "terraform-aws-modules/vpc/aws"
	// version: "5.19.0"
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Source is the source of the terraform module.
	// +kubebuilder:validation:Required
	Source string `json:"source"`
	// Version is the version of the terraform module.
	// +kubebuilder:validation:Optional
	Version string `json:"version,omitempty"`

	// Inputs are the inputs to the terraform module.
	// +kubebuilder:validation:Optional
	Inputs *apiextensionsv1.JSON `json:"inputs,omitempty"`
	// Outputs are the outputs of the terraform module.
	Outputs []ModuleOutput `json:"outputs,omitempty"`
}

// TFSpec defines the config options for executing terraform.
type TFSpec struct {
	// Env is a list of environment variables to set for the terraform process
	// +kubebuilder:validation:Required
	Env []EnvVar `json:"env,omitempty"`
}

// AWSAuthConfig defines the AWS authentication configuration
type AWSAuthConfig struct {
	// ServiceAccountName is the name of the ServiceAccount to use for AWS authentication
	// The ServiceAccount must be in the same namespace as the Workspace
	// +kubebuilder:validation:Required
	ServiceAccountName string `json:"serviceAccountName"`

	// RoleARN is the ARN of the AWS IAM role to assume
	// +kubebuilder:validation:Required
	RoleARN string `json:"roleARN"`
}

// TokenAuthConfig defines the token authentication configuration for arbitrary tokens used for authentication
type TokenAuthConfig struct {
	// SecretKeyRef is a reference to a secret containing a token for authentication
	// +kubebuilder:validation:Required
	SecretKeyRef SecretKeySelector `json:"secretKeyRef"`

	// FilePathEnv is the environment variable to set with the temporary path to the token file
	// +kubebuilder:validation:Required
	FilePathEnv string `json:"filePathEnv"`
}

// AuthenticationSpec defines the authentication configuration for the workspace
type AuthenticationSpec struct {
	// AWS authentication configuration
	// +kubebuilder:validation:Optional
	AWS *AWSAuthConfig `json:"aws,omitempty"`

	// Tokens authentication configuration
	// +kubebuilder:validation:Optional
	Tokens []TokenAuthConfig `json:"tokens,omitempty"`
}

// WorkspaceSpec defines the desired state of Workspace.
type WorkspaceSpec struct {
	// TerraformVersion is the version of terraform to use
	// +kubebuilder:validation:Required
	TerraformVersion string `json:"terraformVersion"`

	// Backend is the backend configuration for the workspace
	// +kubebuilder:validation:Required
	Backend BackendSpec `json:"backend"`

	// ProviderSpecs is a list of provider specifications
	// +kubebuilder:validation:Required
	ProviderSpecs []ProviderSpec `json:"providerSpecs"`

	// Module is the module configuration for the workspace
	// +kubebuilder:validation:Required
	Module *ModuleSpec `json:"module"`

	// TFExec is the terraform execution configuration
	// +kubebuilder:validation:Optional
	TFExec *TFSpec `json:"tf,omitempty"`

	// AutoApply is a flag to indicate if the workspace should be automatically applied
	// +kubebuilder:default=false
	AutoApply bool `json:"autoApply"`

	// PreventDestroy is a flag to indicate if terraform destroy should be skipped when the resource is deleted
	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	// +kubebuilder:deprecatedversion:warning="PreventDestroy has been replaced by Destroy"
	PreventDestroy bool `json:"preventDestroy"`

	// Destroy indicates how to react when the Workspace is deleted, it can
	// either do nothing, automatically run the `terraform destroy` action or
	// await a manual destroy request.
	// +required
	// +kubebuilder:default=skip
	// +kubebuilder:validation:Enum=auto;manual;skip
	Destroy DestroyBehaviour `json:"destroy"`

	// TerraformRC contains the content of the .terraformrc file
	// +kubebuilder:validation:Optional
	TerraformRC string `json:"terraformRC,omitempty"`

	// Authentication is the authentication configuration for the workspace
	// +kubebuilder:validation:Optional
	Authentication *AuthenticationSpec `json:"authentication,omitempty"`

	// PlanHistoryLimit is the number of plans to keep for the workspace
	// +kubebuilder:default=3
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	PlanHistoryLimit int32 `json:"planHistoryLimit,omitempty"`
}

type BackoffStatus struct {
	NextRetryTime *metav1.Time `json:"nextRetryTime,omitempty"`
	RetryCount    int32        `json:"retryCount,omitempty"`
}

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// CurrentPlan is a reference to the current Plan resource
	CurrentPlan *PlanReference `json:"currentPlan,omitempty"`
	// CurrentRender is the current render of the workspace
	// +kubebuilder:validation:Optional
	CurrentRender string `json:"currentRender"`
	// ValidRender is the result of the validation of the workspace
	// +kubebuilder:validation:Optional
	ValidRender bool `json:"validRender"`
	// Backoff is a status to control how often we retry running failed plans
	// +kubebuilder:validation:Optional
	Backoff BackoffStatus `json:"backoff"`

	// InitOutput is the result of initialising dependencies in the workspace
	// +kubebuilder:validation:Optional
	InitOutput string `json:"initOutput"`

	// CurrentContentHash is a hash of the .terraform directory content, used to detect
	// changes in modules that are not evident in the workspace spec (e.g. git modules)
	// +kubebuilder:validation:Optional
	CurrentContentHash string `json:"currentContentHash"`
	// NextRefreshTimestamp is the next time the workspace will be refreshed
	// +kubebuilder:validation:Optional
	NextRefreshTimestamp metav1.Time `json:"nextRefreshTimestamp"`
	// ObservedGeneration is the observed generation of the workspace
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration"`

	// NewPlanNeeded indicates whether a new plan is needed
	// +kubebuilder:validation:Optional
	NewPlanNeeded bool `json:"newPlanNeeded"`

	// NewApplyNeeded indicates whether a new apply is needed
	// +kubebuilder:validation:Optional
	NewApplyNeeded bool `json:"newApplyNeeded"`
	// Terraform execution status
	// TerraformPhase represents the current terraform execution phase
	// +kubebuilder:validation:Optional
	TerraformPhase string `json:"terraformPhase,omitempty"`
	// TerraformMessage provides details about the current terraform operation
	// +kubebuilder:validation:Optional
	TerraformMessage string `json:"terraformMessage,omitempty"`
	// LastErrorTime is the timestamp of the last error that occurred
	// +kubebuilder:validation:Optional
	LastErrorTime *metav1.Time `json:"lastErrorTime,omitempty"`
	// LastErrorMessage contains the last error message that occurred, preserved across reconciliations
	// +kubebuilder:validation:Optional
	LastErrorMessage string `json:"lastErrorMessage,omitempty"`
	// HasChanges indicates whether the last plan contained changes
	// +kubebuilder:validation:Optional
	HasChanges bool `json:"hasChanges"`
	// LastExecutionTime is the time the last terraform operation completed
	// +kubebuilder:validation:Optional
	LastExecutionTime *metav1.Time `json:"lastExecutionTime,omitempty"`
	// LastPlanOutput contains the raw terraform plan output
	// +kubebuilder:validation:Optional
	LastPlanOutput string `json:"lastPlanOutput"`
	// LastApplyOutput contains the raw terraform apply output
	// +kubebuilder:validation:Optional
	LastApplyOutput string `json:"lastApplyOutput,omitempty"`

	// LatestPlan is the name of the latest Plan resource created for this workspace
	// +kubebuilder:deprecatedversion:warning="This field is deprecated and will be removed in a future release. Use CurrentPlan instead."
	// +kubebuilder:validation:Optional
	LatestPlan string `json:"latestPlan,omitempty"`
}

// PlanReference contains a reference to a Plan resource
type PlanReference struct {
	// Name of the Plan
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Plan, defaults to the same namespace as the Workspace
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
}

// Workspace is the Schema for the workspaces API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tfws;ws
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

func (w *Workspace) ManualApplyRequested() bool {
	_, ok := w.Annotations[ManualApplyAnnotation]
	return ok
}

func (w *Workspace) ManualDestroyRequested() bool {
	_, ok := w.Annotations[ManualDestroyAnnotation]
	return ok
}

// WorkspaceList contains a list of Workspace.
// +kubebuilder:object:root=true
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workspace{}, &WorkspaceList{})
}
