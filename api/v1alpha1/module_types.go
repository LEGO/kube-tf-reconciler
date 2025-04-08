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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ModuleOutput struct {
	// Name is the name of the output
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ModuleSpec defines the desired state of Module.
type ModuleSpec struct {
	// Source is the source of the terraform module.
	// Example:
	// source:  "terraform-aws-modules/vpc/aws"
	// version: "5.19.0"
	Source string `json:"source"`
	// Version is the version of the terraform module.
	Version string `json:"version,omitempty"`

	// Inputs are the inputs to the terraform module.
	Inputs *apiextensionsv1.JSON `json:"inputs,omitempty"`
	// Outputs are the outputs of the terraform module.
	Outputs []ModuleOutput `json:"outputs,omitempty"`
}

// ModuleStatus defines the observed state of Module.
type ModuleStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Module is the Schema for the modules API.
type Module struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModuleSpec   `json:"spec"`
	Status ModuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModuleList contains a list of Module.
type ModuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Module `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Module{}, &ModuleList{})
}
