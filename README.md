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
