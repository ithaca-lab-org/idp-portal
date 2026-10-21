# idp-operator

Reconciles `TeamNamespace` custom resources (written by portal-api for Order #01) into a guarded namespace. The portal never touches Kubernetes objects directly.

## What it creates

For a cluster-scoped `TeamNamespace` named `<name>`:

| Object | Name | Notes |
|---|---|---|
| Namespace | `<name>` | labels `app.kubernetes.io/managed-by=idp-operator`, `idp.ithaca-idp.com/team-namespace`, `idp.ithaca-idp.com/business-unit`, `istio.io/dataplane-mode=ambient`, PSS enforce `baseline` / warn `restricted`; annotations `idp.ithaca-idp.com/owner`, `idp.ithaca-idp.com/order-id` |
| ResourceQuota | `idp-quota` | sized by `spec.size`; LoadBalancer and NodePort Services capped at 0 |
| LimitRange | `idp-limits` | container defaults and max, sized by `spec.size` |
| NetworkPolicy | `idp-default-deny-ingress`, `idp-allow-same-namespace` | deny all ingress, then allow from pods in the same namespace |
| RoleBinding | `idp-namespace-admin` | ClusterRole `admin` for `User: <spec.owner>` |

Child specs are overwritten on every reconcile, so manual edits are reverted. Extra labels on the namespace are kept. Deleting the CR deletes the namespace only if it still carries this CR's labels.

## CRD fields (`idp.ithaca-idp.com/v1alpha1`)

| Field | Rules |
|---|---|
| `metadata.name` | DNS-1123 label, at most 40 chars, not reserved (`default`, `kube-*`, `istio-*`, `gke-*`, `argocd`, `cert-manager`, `tailscale`, `identity`, `idp-system`, `echo`) |
| `spec.businessUnit` | required DNS label, immutable |
| `spec.owner` | required email (CEL), mutable (ownership transfer) |
| `spec.size` | `small` (default) or `medium` |
| `spec.orderId` | optional, immutable once set |
| `status` | `phase` (Pending, Ready, Failed), `observedGeneration`, `namespace`, `conditions[type=Ready]` |

Failure reasons on the `Ready` condition: `ReservedName`, `NamespaceConflict` (a namespace with that name exists and is not managed by this CR), `NamespaceTerminating`, `ReconcileError`.

## Develop

```sh
make test     # controller-gen (deepcopy, CRD, RBAC), fetch envtest binaries into ./bin, go test
make check    # gofmt, go vet, test, govulncheck
```

Tool versions are pinned in the `Makefile` and run with `go run`. Generated files (`api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases`, `config/rbac/role.yaml`) are committed. After `make envtest`, plain `go test ./...` also finds the binaries; without them the envtest tests skip.

## RBAC rationale

- The child objects live in namespaces created at runtime, so `create` on resourcequotas, limitranges, networkpolicies and rolebindings is cluster-wide. `update` (and `delete` on rolebindings) is limited by `resourceNames` to the operator's fixed child names.
- Creating a RoleBinding to `admin` requires holding every permission in `admin` or `bind` on it. The operator gets `bind` on `clusterroles` with `resourceNames: [admin]` only; no `escalate`, no wildcards.
- Namespaces need `create`, `update` and `delete` cluster-wide; RBAC cannot scope them by label, so the label check in code is the guard.
- `tokenreviews`/`subjectaccessreviews` `create` protect the HTTPS metrics endpoint; leases and events are a namespaced Role in `idp-system` for leader election.

## Known limits

- RBAC alone would let a compromised operator create objects (including `admin` RoleBindings) in any namespace and delete any namespace. A ValidatingAdmissionPolicy that pins the operator's ServiceAccount to namespaces labelled `managed-by=idp-operator` is planned with the deployment manifests.
- Anyone who can label namespaces can make an existing namespace adoptable by adding the two operator labels.
- The informer cache watches all namespaces, quotas, limit ranges, network policies and role bindings cluster-wide.
- The finalizer is removed once the namespace delete is accepted, not when it has finished terminating; a CR recreated meanwhile waits in `Pending`.
- In ambient mode, traffic from the Istio ingress gateway or waypoints is blocked by the default-deny policy until a later order adds an allow rule.
- Grafana folder and Vault path/policy/group from the design are not implemented yet.
