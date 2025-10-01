package main

import "github.com/LEGO/kube-tf-reconciler/cmd"

// Generate CRDs
//go:generate go tool controller-gen rbac:roleName=manager-role crd:maxDescLen=1024 object paths="./api/..." output:crd:dir=crds
//go:generate bash -c "cp crds/*.yaml charts/terraform-reconciler/templates/"
// Prepare testing executables
//go:generate go tool setup-envtest use 1.32.0 --bin-dir bin/ -p path

func main() {
	cmd.Execute()
}
