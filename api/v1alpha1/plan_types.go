package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlanPhase represents the current phase of the Plan
type PlanPhase string

const (
	// PlanPhasePending indicates the plan is waiting to be executed
	PlanPhasePending PlanPhase = "Pending"
	// PlanPhasePlanning indicates the plan is currently being generated
	PlanPhasePlanning PlanPhase = "Planning"
	// PlanPhasePlanned indicates the plan has been successfully generated
	PlanPhasePlanned PlanPhase = "Planned"
	// PlanPhaseApplying indicates the plan is currently being applied
	PlanPhaseApplying PlanPhase = "Applying"
	// PlanPhaseApplied indicates the plan has been successfully applied
	PlanPhaseApplied PlanPhase = "Applied"
	// PlanPhaseErrored indicates the plan encountered an error
	PlanPhaseErrored PlanPhase = "Errored"
	// PlanPhaseCancelled indicates the plan was cancelled
	PlanPhaseCancelled PlanPhase = "Cancelled"
)

// PlanSpec defines the desired state of Plan
type PlanSpec struct {
	// WorkspaceRef is a reference to the Workspace that owns this Plan
	// +kubebuilder:validation:Required
	WorkspaceRef WorkspaceReference `json:"workspaceRef"`

	// AutoApply indicates whether this plan should be automatically applied
	// +kubebuilder:default=false
	AutoApply bool `json:"autoApply"`

	// TerraformVersion is the version of terraform to use for this plan
	// +kubebuilder:validation:Required
	TerraformVersion string `json:"terraformVersion"`

	// Render is the HCL content that will be planned
	// +kubebuilder:validation:Required
	Render string `json:"render"`

	// Destroy indicates this is a destroy plan (empty HCL will be used)
	// +kubebuilder:default=false
	Destroy bool `json:"destroy"`
}

// WorkspaceReference contains enough information to let you locate the referenced Workspace
type WorkspaceReference struct {
	// Name of the Workspace
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Workspace, defaults to the same namespace as the Plan
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
}

// PlanStatus defines the observed state of Plan
type PlanStatus struct {
	// Phase represents the current phase of the Plan
	// +kubebuilder:validation:enum=Pending;Planning;Planned;Applying;Applied;Errored;Cancelled
	Phase PlanPhase `json:"phase"`

	// Message provides a human readable message indicating details about the transition
	// +kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`

	// PlanOutput contains the raw terraform plan output
	// +kubebuilder:validation:Optional
	PlanOutput string `json:"planOutput,omitempty"`

	// HasChanges indicates whether this plan contains changes to apply
	// +kubebuilder:validation:Optional
	HasChanges bool `json:"hasChanges"`

	// ValidRender indicates whether the rendered HCL is valid
	// +kubebuilder:validation:Optional
	ValidRender bool `json:"validRender"`

	// StartTime is the time the plan execution started
	// +kubebuilder:validation:Optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is the time the plan execution completed
	// +kubebuilder:validation:Optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// ObservedGeneration is the observed generation of the Plan
	ObservedGeneration int64 `json:"observedGeneration"`
}

// Plan is the Schema for the plans API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tfplan;plan
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="HasChanges",type="boolean",JSONPath=".status.hasChanges"
// +kubebuilder:printcolumn:name="AutoApply",type="boolean",JSONPath=".spec.autoApply"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Plan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlanSpec   `json:"spec,omitempty"`
	Status PlanStatus `json:"status,omitempty"`
}

// PlanList contains a list of Plan
// +kubebuilder:object:root=true
type PlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Plan `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Plan{}, &PlanList{})
}
