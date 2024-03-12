# pv-to-csi

`pv-to-csi` is a small go script that can be used to quickly migrate in-tree volumes to CSI inside a K8s cluster.

## Requirements

This migration rely on a patched apiserver to allow for in-place edits of the PV in the cluster. To deploy this patched apiserver, you __need__ to have access to the ETCD server of your cluster. For self-hosted clusters, this should be fairly easy. However, for managed clusters (AKS, EKS, GKE, ...), it depends on if the provider allows access to the ETCD (unlikely).

Also, until <https://github.com/kubernetes/kubernetes/issues/121107> is resolved, a patch is needed in the `vendor` directory for `csi-translation-lib` with `azuredisk-csi-driver`. The patch is already present in the repository.

## Installation

- clone the repo
- install the tool `go install .`
- add the binary to your PATH `export PATH="$GOPATH/bin:$PATH"`

## Usage

- configure the patched apiserver (see [patched-api-server](#patched-api-server)). Note the ip address or domain name of the patched apiserver
- run the migration `pv-to-csi -context=my-cluster -patched-api=<ip or domain name> -dry-run` (remove `-dry-run` to run the migration for real)
- it is advised to run the migration with `-f=<some file>` to create a backup file that can be used with `-rollback` in case there is an issue with the migration

### Supported CSI drivers

The script was tested against real clusters using the following CSI drivers:

- [aws-ebs-csi-driver](https://github.com/kubernetes-sigs/aws-ebs-csi-driver)
- [gcp-compute-persistent-disk-csi-driver](https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver)
- [azuredisk-csi-driver](https://github.com/kubernetes-sigs/azuredisk-csi-driver)

For other drivers, the compatibility was not tested.

### Patched api server

The migration tool uses a modified apiserver to be able to modify PV objects in place. This requires build a custom apiserver with a custom [patch](./kubernetes.patch).

- The patch was created using `git format-patch` so it can be applied using `git am kubernetes.patch` on any fork of the apiserver in [k/k](https://github.com/kubernetes/kubernetes).
- As deploying the Apiserver depends a lot on the internal workings of the K8s clusters of each individual, we won't give any instructions on how to deploy it. The server can be deployed in place of the existing apiservers or alongside them. The second option is preferrable to not disable Apiserver validations for regular users not needing the patch.
- Once the apiserver is deployed, it needs 2 things:
  - access to the ETCD
  - be accessible with a Kubernetes client (like `kubectl` or `client-go`)
