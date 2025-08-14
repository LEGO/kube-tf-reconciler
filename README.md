# Kube Terraform Reconciler

**NOTE**: This project is currently developed for internal use, but may later be developed for broader consumption

Kube Terraform Reconciler (krec) is a Kubernetes operator for managing infrastructure as code using Terraform. It allows you to define Terraform workspaces as Kubernetes custom resources and automatically reconciles your infrastructure based on these resources.

Features
- Define Terraform workspaces as Kubernetes resources
- Automatic reconciliation of infrastructure
- Support for custom providers and modules
- Terraform backend configuration
- Auto-apply functionality
- State tracking through Kubernetes status

## Usage

To deploy the operator, the recommended approach is using the provided Helm chart under /charts/terraform-reconciller

After deploying the operator, get started by deploying a Workspace resource. A workspace is a completely self-contained Terraform execution environment, including the Terraform version, authentication, backend configuration, provider specifications, and module definitions.

A simple example manifest would look like this:

```yaml
apiVersion: tf-reconcile.lego.com/v1alpha1
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

### Authentication towards private module repositories

If the Terraform module resides in a private repository (like a private GitHub repository), the workspace needs addditional environment variables to use for authentication.

```yaml
apiVersion: tf-reconcile.lego.com/v1alpha1
kind: Workspace
metadata:
  name: workspace1
spec:
  terraformVersion: 1.11.2
  tf:
    env:
      - name: GIT_CONFIG_COUNT
        value: "1"
      - name: GIT_CONFIG_KEY_0
        secretKeyRef:
          name: github-credentials
          key: github-token-url
      - name: GIT_CONFIG_VALUE_0
        value: "https://github.com/"
```

where the GitHub credentials would look like:
```
url.https://oauth2:github_pat_xxxxxxxx@github.com/.insteadOf
```

### Authentication towards cloud providers
In the simple example above, authentication credentials are stored in a Kubernetes secret.
A more secure approach however would be to set up OIDC authentication for your Kubernetes cluster and use service accounts with the appropriate permissions.

For example, granting admin access in AWS would look something like this:

```hcl
resource "aws_iam_openid_connect_provider" "my_cluster" {
  url = "cluster_url"
  client_id_list = ["client_id"]
  thumbprint_list = ["thumbprint"]
}

resource "aws_iam_role" "tf_reconciler_role" {
  name = "terraform-reconciler-service-account-admin-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17",
    Statement = [
      {
        Effect = "Allow",
        Principal = {
          Federated = aws_iam_openid_connect_provider.my_cluster.arn
        },
        Action = "sts:AssumeRoleWithWebIdentity",
        Condition = {
          StringEquals = {
            "cluster_url:sub" = "system:serviceaccount:namespace_name:service_account_name"
          }
        }
      }
    ]
  })
}

data "aws_iam_policy" "admin" {
  name = "AdministratorAccess"
}

resource "aws_iam_role_policy_attachment" "admin_policy_attachment" {
  role       = aws_iam_role.tf_reconciler_role.name
  policy_arn = data.aws_iam_policy.admin.arn
}
```

Then, the Workspace can assume the role and access the necessary resources using the following parameters

```yaml
apiVersion: tf-reconcile.lego.com/v1alpha1
kind: Workspace
metadata:
  name: workspace1
spec:
  authentication:
    aws:
      roleARN: arn:aws:iam::account_id:role/role_name
      serviceAccountName: service_account_name
```

## Contributors Guide

Ensure CRDs are updated by running the following command:

```bash
go generate ./...
```

Running tests

```bash
go test ./... -v
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
