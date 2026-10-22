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

## Admission policy

`config/policy/operator-scope.yaml` holds a `ValidatingAdmissionPolicy` and its binding (`idp-operator-scope`, `failurePolicy: Fail`, action `Deny`). `matchConditions` limit it to `system:serviceaccount:idp-system:idp-operator`, so other users are not evaluated. Deploy it before (or with) the operator. For that user it enforces:

- Namespaces: `CREATE` only with `managed-by=idp-operator` and `team-namespace=<name>`, and not for a reserved name; `UPDATE`/`DELETE` only if the stored object (`oldObject`) already carries both labels, and an `UPDATE` cannot change or drop them. PSS `enforce` must stay `baseline` or `restricted`. Subresources are included.
- ResourceQuotas, LimitRanges, NetworkPolicies, RoleBindings: only in namespaces carrying both labels, and only under the fixed child names.
- RoleBindings: roleRef `ClusterRole admin`, exactly one `User` subject that contains `@` and does not start with `system:` (no groups, ServiceAccounts or `system:serviceaccount:...` users).

What it does not protect:

- Anyone with cluster-admin, or with write access to `validatingadmissionpolicies`/`validatingadmissionpolicybindings`, can delete or edit the policy. Renaming the ServiceAccount or its namespace silently disables it.
- Other users labelling a namespace to hand it to the operator (see Known limits).
- Inside managed namespaces the operator can still delete them, rewrite quotas and limits, and grant `admin` to any email-looking user. It can also create labelled namespaces under any non-reserved name (squatting a future team name).
- `namespaceObject` comes from the apiserver's informer cache, so for a short window after an admin removes the labels the operator may still write children in that namespace.
- Writes to other kinds are left to RBAC (the policy is not an allowlist of everything the operator can touch).

`internal/controller/policy_envtest_test.go` installs `config/rbac` and `config/policy` in envtest (RBAC on), impersonates the ServiceAccount, runs the reconciler end to end, checks the denials with server-side dry-run, and removes the binding to show those denials come from the policy and not RBAC.

## Known limits

- RBAC alone would let a compromised operator create objects (including `admin` RoleBindings) in any namespace and delete any namespace; the admission policy below closes this.
- Anyone (other than the operator) who can label namespaces can make an existing namespace adoptable by adding the two operator labels. The policy cannot stop this; restrict namespace `update`/`patch` to platform admins.
- The informer cache watches all namespaces, quotas, limit ranges, network policies and role bindings cluster-wide.
- The finalizer is removed once the namespace delete is accepted, not when it has finished terminating; a CR recreated meanwhile waits in `Pending`.
- In ambient mode, traffic from the Istio ingress gateway or waypoints is blocked by the default-deny policy until a later order adds an allow rule.
- Grafana folder and Vault path/policy/group from the design are not implemented yet.
