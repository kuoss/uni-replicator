# uni-replicator

`uni-replicator` continuously replicates configured namespaced Kubernetes resources to explicitly selected namespaces within the same cluster. It discovers resources at runtime, so it supports CRDs without requiring resource-specific Go types or clients.

## How it works

At startup, every configured `apiVersion` + `resources` entry is validated through Kubernetes discovery and resolved to a GroupVersionResource. One dynamic informer watches each resolved resource across all namespaces. Events only enqueue work; rate-limited workers perform reconciliation.

Add this annotation to a source object:

```yaml
metadata:
  annotations:
    replicator.kuoss.github.io/replicate-to: "app-a,app-b"
```

The controller creates same-named objects in `app-a` and `app-b`, updates them when the source changes, recreates deleted replicas, and deletes replicas when a destination is removed or the source is deleted. The source namespace is ignored if listed.

Replicas are marked with:

```yaml
metadata:
  labels:
    replicator.kuoss.github.io/managed: "true"
  annotations:
    replicator.kuoss.github.io/source-namespace: platform
    replicator.kuoss.github.io/source-hash: "..."
```

Lifecycle/server fields, owner references, finalizers, and `status` are removed before replication. Synchronization uses server-side apply with field manager `uni-replicator` and `force=false`. An existing destination object is never overwritten unless it is already marked as managed by the same source. SSA ownership conflicts are logged and retried without forcing ownership.

## Configuration

Create a YAML file containing the API resources to watch:

```yaml
behavior:
  cascadeDeletionPolicy: Delete # Delete or Retain

watches:
  - apiVersion: v1
    resources:
      - secrets
  - apiVersion: k8s.nginx.org/v1
    resources:
      - policies
```

Resource names are the plural API resource names reported by Kubernetes discovery, such as `secrets` or `policies`. All entries must exist at the configured API version and must be namespaced. A missing CRD, resource, or API version and a cluster-scoped resource cause startup to fail clearly. Configuration is read only at startup.

`behavior.cascadeDeletionPolicy` defaults to `Delete`. `Retain` preserves replicas when their source is deleted, removes their controller-managed metadata, and leaves them as unmanaged objects. Removing a namespace from `replicate-to` still deletes its replica, and directly deleting a destination still causes recreation.

The configuration file is selected in this order: `--config`, the `UNI_REPLICATOR_CONFIG` environment variable, `./config.yaml`, `./etc/config.yaml`, then `/etc/uni-replicator/config.yaml`. An explicitly selected path is authoritative. Automatic discovery uses the first existing file, and a missing or invalid selected file causes startup to fail without falling through to another file. The selected absolute path is logged at startup.

Run locally with a kubeconfig:

```bash
go run ./cmd/uni-replicator \
  --config ./config/example.yaml \
  --kubeconfig "$KUBECONFIG"
```

Kubernetes client configuration is selected in this order: `--kubeconfig`, the `KUBECONFIG` environment variable, in-cluster configuration, then `~/.kube/config`. An explicitly selected kubeconfig is authoritative, so an invalid `--kubeconfig` or `KUBECONFIG` value causes startup to fail instead of silently falling back. Other useful flags are `--workers` (default `2`) and `--resync-period` (default `10m`).

## NGINX Policy example

Install the NGINX CRDs first, then configure:

```yaml
watches:
  - apiVersion: k8s.nginx.org/v1
    resources:
      - policies
```

The controller discovers `policies.k8s.nginx.org` dynamically. The following object is replicated without importing an NGINX Go package:

```yaml
apiVersion: k8s.nginx.org/v1
kind: Policy
metadata:
  name: common-policy
  namespace: platform
  annotations:
    replicator.kuoss.github.io/replicate-to: "app-a,app-b"
spec:
  rateLimit:
    rate: 10r/s
    key: ${binary_remote_addr}
    zoneSize: 10M
```

## Install

Install a published release directly from the OCI chart in GitHub Container Registry:

```bash
helm install uni-replicator \
  oci://ghcr.io/kuoss/charts/uni-replicator \
  --version 0.1.0 \
  --namespace uni-replicator \
  --create-namespace
```

See the [Helm chart documentation](charts/uni-replicator/README.md) for values, automatic RBAC generation, upgrades, and local rendering.

Alternatively, build the image and apply the static manifest:

```bash
docker build -t ghcr.io/kuoss/uni-replicator:latest .
kubectl apply -f manifests/install.yaml
```

Keep static-manifest RBAC synchronized with its ConfigMap when changing watched resources. The Helm chart handles this automatically.

## Kubernetes permissions

At startup, the controller verifies that its identity has `get`, `list`, `watch`, `patch`, and `delete` access to every configured resource and exits if any required permission is missing. Permission or API errors that occur after startup are logged and retried without terminating the controller.

The controller also performs a best-effort check for wildcard resource permissions across all API groups. When found, it logs a warning followed by a copyable, unescaped least-privilege ClusterRole YAML generated from the resources resolved from the active configuration. This warning is advisory and does not prevent startup.

## Release

Pushing a semantic version tag such as `v0.1.0` runs the release workflow. It tests the Go code, lints the chart, publishes the controller image to `ghcr.io/kuoss/uni-replicator`, and pushes the versioned chart to `oci://ghcr.io/kuoss/charts/uni-replicator`.

```bash
git tag v0.1.0
git push origin v0.1.0
```

GHCR packages are private when first created. Set both the `uni-replicator` image package and `charts/uni-replicator` chart package to public in the GitHub package settings to allow unauthenticated installation.

## Generic CRD smoke test

The repository includes a deliberately unrelated `Widget` CRD to demonstrate that no compile-time Go type is required:

```bash
kubectl create namespace platform
kubectl create namespace app-a
kubectl create namespace app-b
kubectl apply -f testdata/widget-crd.yaml
```

Add the Widget to the controller configuration and restart it:

```yaml
watches:
  - apiVersion: replication-test.kuoss.io/v1
    resources:
      - widgets
```

Then create the source and inspect both replicas:

```bash
kubectl apply -f testdata/widget-source.yaml
kubectl get widgets.replication-test.kuoss.io -A
```

The automated controller test performs the same lifecycle against the dynamic client: generic CRD creation and update, replica recreation after deletion, destination removal, source deletion, and unmanaged-object protection.

## Development

```bash
gofmt -w .
go test ./...
go vet ./...
go build ./...
```

Render or lint the Helm chart locally:

```bash
make helm-template
```

See the [Helm chart documentation](charts/uni-replicator/README.md#render-locally) for custom values and available Make variables.

Requires Go 1.25 or newer.
