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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderRef is a reference to a provider
type ProviderRef struct {
	// Name is the name of the provider
	Name string `json:"name"`
	// Namespace is the namespace of the provider
	Namespace string `json:"namespace,omitempty"`
}

// BackendSpec defines the backend configuration for the workspace
type BackendSpec struct {
	// Type is the type of the backend
	// +kubebuilder:validation:Enum=local;remote;s3;gcs;azurerm;oss;consul;cos;http;pg;kubernetes
	Type string `json:"type"`
	// Inputs are the inputs to the terraform module.
	Inputs *apiextensionsv1.JSON `json:"inputs,omitempty"`
}

// WorkspaceSpec defines the desired state of Workspace.
type WorkspaceSpec struct {
	// Backend is the backend configuration for the workspace
	// +kubebuilder:validation:Required
	Backend BackendSpec `json:"backend"`

	// ProviderRefs is a list of provider references
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinSize=1
	ProviderRefs []ProviderRef `json:"providerRefs,omitempty"`

	// Module is the module configuration for the workspace
	// +kubebuilder:validation:Required
	Module *ModuleSpec `json:"module"`

	// WorkerSpec is the worker configuration for the workspace
	WorkerSpec *v1.PodSpec `json:"workerSpec,omitempty"`
}

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// Workspace is the Schema for the workspaces API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
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
