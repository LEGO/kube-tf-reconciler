<<<<<<< debug-config
# Kube Terraform Reconciler

Kube Terraform Reconciler (krec) is a Kubernetes operator for managing infrastructure as code using Terraform. It allows you to define Terraform workspaces as Kubernetes custom resources and automatically reconciles your infrastructure based on these resources.

Features
- Define Terraform workspaces as Kubernetes resources
- Automatic reconciliation of infrastructure
- Support for custom providers and modules
- Terraform backend configuration
- Auto-apply functionality
- State tracking through Kubernetes status


## Usage

Create a Workspace resource:

```yaml
apiVersion: tf-reconcile.lukaspj.io/v1alpha1
kind: Workspace
metadata:
  name: workspace1
spec:
  terraformVersion: 1.11.2
  tf:
    env:
      - name: AWS_REGION
        value: eu-west-1
      - name: AWS_ACCESS_KEY_ID
        secretKeyRef:
          name: aws-access-key
          key: access-key-id
      - name: AWS_SECRET_ACCESS_KEY
        secretKeyRef:
          name: aws-access-key
          key: secret-access-key
      - name: AWS_SESSION_TOKEN
        secretKeyRef:
          name: aws-access-key
          key: session-token
  backend:
    type: local
  providerSpecs:
    - name: aws
      source: hashicorp/aws
      version: 5.94.1
  module:
    name: my-module
    source: terraform-aws-modules/iam/aws//modules/iam-read-only-policy
    inputs:
      name: "awesome-role-krec-testing"
      path: "/"
      description: "My example read-only policy"
      allowed_services: ["rds", "dynamo"]
```


## Debugging in a live cluster

To debug the operator locally:

1. build the debug image:
```bash
docker build -f Dockerfile.debug -t krec:debug .
```

*ALTERNATIVELY, with minikube it's possible to directly build images into minikube's internal Docker Engine, which makes step 2 unneccessary*
```bash
eval $(minikube docker-env)
docker build -f Dockerfile.debug -t krec:debug .
```

2. Load the image into your local Kubernetes cluster:
```bash
# For KIND
kind load docker-image krec:debug

# For Minikube
minikube image load krec:debug
```

3. Deploy the operator with the debug image (make sure the CRD is installed beforehand):
```bash
kubectl apply -f samples/debug-deployment.yaml
```

4. Set up port forwarding:
```bash
kubectl port-forward -n krec-debug svc/krec-debug 2345:2345
```

5. Connect your debugger:

- For VS Code: Configure launch.json to connect to localhost:2345
- For GoLand: Set up a Go Remote configuration targeting localhost:2345
- For Delve CLI: dlv connect localhost:2345
=======
# Kube TF Reconciler

**NOTE**: This project is currently developed for internal use, but may later be developed for broader consumption

The Kube TF Reconciler is a kubernetes operator that uses a regular terraform module as input and reconciles all the
resources defined in the module. It's designed to be multitenant meaning that terraform operations will be executed
in isolation from each other, so you can run multiple reconciliations in parallel without worrying about leaking credentials, state etc.

## Getting started

Updating CRD's by running the following command:

```bash
go generate ./...
```

Running tests

```bash
go test ./... -v
```

## License

This project is licensed under Apache License 2.0. See the [LICENSE](LICENSE)

### MPL Dependencies

This project includes the following dependencies that are licensed under the Mozilla Public License (MPL):

- [https://github.com/hashicorp/go-version](https://github.com/hashicorp/go-version)
- [https://github.com/hashicorp/hc-install](https://github.com/hashicorp/hc-install)
- [https://github.com/hashicorp/hcl](https://github.com/hashicorp/hcl/v2)
- [https://github.com/hashicorp/terraform-exec](https://github.com/hashicorp/terraform-exec)

#### MPL Modifications

If you change any MPL-2.0 files, you must distribute those modified files under the MPL-2.0.
The full text of the MPL licenses can be found in the [LICENSES folder](LICENSES). Its recommended to look at the licenses stipulated in the repositories of the dependencies as listed above to make sure they are up to date.

## Contributions

We welcome contributions to the Kube TF Reconciler, please read the [contribution guidelines](./CONTRIBUTING.md) for more information on how to contribute.

As this project is in the early stages of development, we are still working on the contribution guidelines and best practices.
We appreciate your patience and understanding as we work to improve the project.
>>>>>>> main
