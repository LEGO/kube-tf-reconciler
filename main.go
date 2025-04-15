package main

import "lukaspj.io/kube-tf-reconciler/cmd"

// Generate CRDs
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2 rbac:roleName=manager-role crd:maxDescLen=0 object paths="./api/..." output:crd:dir=crds
// Prepare testing executables
//go:generate go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.32.0 --bin-dir bin/ -p path

var (
	version = "dev"
	sha     = "none"
)

func main() {
	cmd.Execute()
}
