# Kube TF Reconciler

**NOTE**: This project is currently in an early stage of development and is not yet ready for production use.

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

## Contributions

We welcome contributions to the Kube TF Reconciler, please read the [contribution guidelines](./CONTRIBUTING.md) for more information on how to contribute.

As this project is in the early stages of development, we are still working on the contribution guidelines and best practices.
We appreciate your patience and understanding as we work to improve the project.