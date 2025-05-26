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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tfreconcilev1alpha1 "lukaspj.io/kube-tf-reconciler/api/v1alpha1"
)

var _ = Describe("Workspace Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		workspace := &tfreconcilev1alpha1.Workspace{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Workspace")
			err := k8sClient.Get(ctx, typeNamespacedName, workspace)
			if err != nil && errors.IsNotFound(err) {
				resource := &tfreconcilev1alpha1.Workspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: tfreconcilev1alpha1.WorkspaceSpec{
						Backend: tfreconcilev1alpha1.BackendSpec{
							Type: "s3",
							Inputs: &apiextensionsv1.JSON{
								Raw: []byte(`{"bucket": "my-bucket"}`),
							},
						},
						ProviderRefs: []tfreconcilev1alpha1.ProviderRef{
							{
								Name:      "aws",
								Namespace: "default",
							},
						},
						Module: &tfreconcilev1alpha1.ModuleSpec{
							Source:  "terraform-aws-modules/vpc/aws",
							Version: "5.19.0",
						},
						WorkerSpec: &v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:       "worker",
									Image:      "hashicorp/terraform:1.11",
									Args:       []string{"version"},
									WorkingDir: "/workspace",
									EnvFrom:    nil,
									Env:        nil,
									Resources:  v1.ResourceRequirements{},
									VolumeMounts: []v1.VolumeMount{
										{
											Name:      "workspace",
											MountPath: "/workspace",
										},
									},
									LivenessProbe:   nil,
									ReadinessProbe:  nil,
									StartupProbe:    nil,
									Lifecycle:       nil,
									ImagePullPolicy: v1.PullIfNotPresent,
									SecurityContext: nil,
								},
							},
							Volumes: []v1.Volume{
								{
									Name: "workspace",
									VolumeSource: v1.VolumeSource{
										ConfigMap: &v1.ConfigMapVolumeSource{
											LocalObjectReference: v1.LocalObjectReference{
												Name: "workspace-config",
											},
										},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &tfreconcilev1alpha1.Workspace{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Workspace")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
