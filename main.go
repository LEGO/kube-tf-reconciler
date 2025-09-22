package main

import "github.com/LEGO/kube-tf-reconciler/cmd"

// Generate CRDs
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2 rbac:roleName=manager-role crd:maxDescLen=1024 object paths="./api/..." output:crd:dir=crds
//go:generate bash -c "mkdir -p charts/terraform-reconciler/crds && cp crds/*.yaml charts/terraform-reconciler/crds/"
// Prepare testing executables
//go:generate go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.32.0 --bin-dir bin/ -p path

func main() {
	cmd.Execute()
}
