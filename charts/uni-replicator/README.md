# uni-replicator Helm chart

This chart installs [uni-replicator](../../README.md), including its ServiceAccount, configuration, least-privilege ClusterRole, ClusterRoleBinding, and Deployment.

## Install

Install a published chart from GitHub Container Registry:

```bash
helm install uni-replicator \
  oci://ghcr.io/kuoss/charts/uni-replicator \
  --namespace uni-replicator \
  --create-namespace
```

## Configure watched resources

The `config` value is rendered to `/etc/uni-replicator/config.yaml` in the controller container. The chart derives ClusterRole rules from the same `config.watches` entries, keeping application configuration and RBAC synchronized.

```yaml
config:
  policy:
    cascadeDeletion: Delete
    existingTarget: Preserve

  watches:
    - apiVersion: v1
      resources:
        - secrets
        - configmaps

    - apiVersion: k8s.nginx.org/v1
      resources:
        - policies
```

Install CRDs before configuring their resources. The controller exits during startup if an API version or resource does not exist or if required permissions are missing.

`config.policy.cascadeDeletion` controls what happens when a source is deleted:

- `Delete` removes its managed replicas and is the default.
- `Retain` preserves its replicas, removes controller metadata and the replication annotation, and leaves them as unmanaged objects.

This policy does not affect target removal from `replicate-to`, which always deletes the corresponding replica. A target deleted directly is always recreated.

`config.policy.existingTarget` controls what happens when a target already contains an object with the source object's name:

- `Preserve` leaves objects not managed by the same source untouched and is the default.
- `Overwrite` takes over the object with server-side apply and forces ownership of conflicting fields.

Apply custom values with:

```bash
helm upgrade --install uni-replicator \
  oci://ghcr.io/kuoss/charts/uni-replicator \
  --namespace uni-replicator \
  --create-namespace \
  --values values.yaml
```

## RBAC and ServiceAccount

By default, the chart creates a ClusterRole containing `get`, `list`, `watch`, `patch`, and `delete` for every configured resource. It also creates and binds a ServiceAccount.

To manage these externally:

```yaml
rbac:
  create: false

serviceAccount:
  create: false
  name: existing-service-account
```

The external identity must have all required permissions across namespaces.

## Values

| Value | Default | Description |
| --- | --- | --- |
| `replicaCount` | `1` | Number of controller replicas |
| `image.repository` | `ghcr.io/kuoss/uni-replicator` | Controller image repository |
| `image.tag` | chart `appVersion` | Controller image tag |
| `image.pullPolicy` | `IfNotPresent` | Kubernetes image pull policy |
| `imagePullSecrets` | `[]` | Image pull secret references |
| `serviceAccount.create` | `true` | Create a ServiceAccount |
| `serviceAccount.name` | generated | Existing or custom ServiceAccount name |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations |
| `rbac.create` | `true` | Create ClusterRole and ClusterRoleBinding |
| `config.policy.cascadeDeletion` | `Delete` | Delete or retain replicas when their source is deleted |
| `config.policy.existingTarget` | `Preserve` | Preserve or overwrite an existing target object |
| `config.watches` | `v1/secrets,configmaps` | API versions and resources to replicate |
| `controller.workers` | `2` | Reconciliation worker count |
| `controller.resyncPeriod` | `10m` | Informer resync period |
| `resources` | small requests/limits | Controller CPU and memory resources |
| `podAnnotations` | `{}` | Additional Pod annotations |
| `podLabels` | `{}` | Additional Pod labels |
| `podSecurityContext` | non-root | Pod-level security context |
| `securityContext` | restricted | Container-level security context |
| `nodeSelector` | `{}` | Pod node selector |
| `tolerations` | `[]` | Pod tolerations |
| `affinity` | `{}` | Pod affinity rules |

See [values.yaml](values.yaml) for the complete defaults and commented configuration examples.

## Render locally

From the repository root:

```bash
make helm-template
make helm-template VALUES=my-values.yaml
make helm-template VALUES=my-values.yaml > rendered.yaml
make helm-lint VALUES=my-values.yaml
```

`RELEASE_NAME`, `NAMESPACE`, `CHART_DIR`, and `HELM_ARGS` can be overridden as Make variables.

The equivalent direct Helm command is:

```bash
helm template uni-replicator charts/uni-replicator \
  --namespace uni-replicator \
  --values my-values.yaml
```

## Uninstall

```bash
helm uninstall uni-replicator --namespace uni-replicator
```
