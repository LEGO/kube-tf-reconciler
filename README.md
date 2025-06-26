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
