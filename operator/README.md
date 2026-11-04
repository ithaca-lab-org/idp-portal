# idp-operator

Reconciles `TeamNamespace` custom resources (written by portal-api for Order #01) into a guarded namespace, and `WorkloadIdentity` resources (Order #03) into a per-workload identity inside it. The portal never touches Kubernetes objects directly.

## What it creates

For a cluster-scoped `TeamNamespace` named `<name>`:

| Object | Name | Notes |
|---|---|---|
| Namespace | `<name>` | labels `app.kubernetes.io/managed-by=idp-operator`, `idp.ithaca-idp.com/team-namespace`, `idp.ithaca-idp.com/business-unit`, `istio.io/dataplane-mode=ambient`, `ithaca.idp/gateway-access=allowed` (lets HTTPRoutes attach to the https listener of Gateway `istio-ingress/ingress`; namespaces created earlier get it on their next reconcile), PSS enforce `baseline` / warn `restricted`; annotations `idp.ithaca-idp.com/owner`, `idp.ithaca-idp.com/order-id` |
| ResourceQuota | `idp-quota` | sized by `spec.size`; LoadBalancer and NodePort Services capped at 0 |
| LimitRange | `idp-limits` | container defaults and max, sized by `spec.size` |
| NetworkPolicy | `idp-default-deny-ingress`, `idp-allow-same-namespace`, `idp-allow-ingress-gateway` | deny all ingress, then allow from pods in the same namespace and from the ingress Gateway pods (namespace `istio-ingress`, label `gateway.networking.k8s.io/gateway-name=ingress`, one peer so both selectors apply, TCP 15008 only: the gateway reaches ambient workloads through HBONE on 15008, so non-ambient tenant pods are intentionally not reachable through it). Existing namespaces get the gateway policy on the next reconcile |
| RoleBinding | `idp-namespace-admin` | ClusterRole `admin` for `User: <spec.owner>` |

With `VAULT_ENABLED=true` (see [Vault access](#vault-access)) two more objects are created in the namespace:

| Object | Name | Notes |
|---|---|---|
| ServiceAccount | `vault-reader` | `automountServiceAccountToken: false`; VSO requests its tokens (audience `vault`) through the TokenRequest API |
| VaultAuth (`secrets.hashicorp.com/v1beta1`) | `vault-reader` | `method: kubernetes`, `mount: kubernetes`, `kubernetes: {role: ns-reader, serviceAccount: vault-reader, audiences: [vault], tokenExpirationSeconds: 600}`, `vaultConnectionRef: vault-secrets-operator/idp-vault`, no `allowedNamespaces` |

Child specs are overwritten on every reconcile, so manual edits are reverted. Extra labels on the namespace are kept. Deleting the CR deletes the namespace only if it still carries this CR's labels.

## CRD fields (`idp.ithaca-idp.com/v1alpha1`)

| Field | Rules |
|---|---|
| `metadata.name` | DNS-1123 label, at most 40 chars, not reserved (`default`, `kube-*`, `istio-*`, `gke-*`, `argocd`, `cert-manager`, `tailscale`, `identity`, `idp-system`, `echo`, `vault`, `vault-*`) |
| `spec.businessUnit` | required DNS label, immutable |
| `spec.owner` | required email (CEL), mutable (ownership transfer) |
| `spec.size` | `small` (default) or `medium` |
| `spec.orderId` | optional, immutable once set |
| `status` | `phase` (Pending, Ready, Failed), `observedGeneration`, `namespace`, `conditions[type=Ready]`; with Vault enabled also `vaultPurgedAt`, `VaultReady` and, while a deletion waits for Vault, `VaultPurgePending` |

Failure reasons on the `Ready` condition: `ReservedName`, `NamespaceConflict` (a namespace with that name exists and is not managed by this CR), `NamespaceTerminating`, `ReconcileError`.

## Vault access

Off by default. With `VAULT_ENABLED=false` the operator creates no Vault objects, adds no Vault finalizer and never calls Vault. The Vault side (policies `ns-reader`/`ns-purge`, roles `ns-reader`/`idp-operator-purge`, the shared VaultConnection) is configured by idp-gitops (`workloads/vault/README.md`, "Per-namespace access"); the operator gets no Vault write access for tenant access.

| Env | Default | Meaning |
|---|---|---|
| `VAULT_ENABLED` | `false` | turn the integration on |
| `VAULT_ADDR` | `https://vault.vault.svc.cluster.local:8200` | Vault API (purge only) |
| `VAULT_PURGE_ROLE` | `idp-operator-purge` | Kubernetes-auth role for the purge login |
| `VAULT_AUTH_MOUNT` | `kubernetes` | auth mount for the purge login (the VaultAuth `mount` is fixed to `kubernetes` by the contract and the admission policy) |
| `VAULT_KV_MOUNT` | `kv` | KV v2 mount holding `<ns>/...` |
| `VAULT_TOKEN_PATH` | `/var/run/secrets/vault/token` | projected ServiceAccount token with audience `vault` |
| `VAULT_CACERT` | (none) | PEM file with the CA of Vault's TLS listener; when set it is the only trusted root for Vault and `VAULT_ADDR` must be `https` (read once at start; empty: system roots) |
| `VAULT_PURGE_GIVE_UP_AFTER` | `30m` | stop retrying a failing purge this long after deletion |

**Purge before access.** Before the ServiceAccount and VaultAuth exist, the operator runs the same purge of `kv/<ns>/` once, so a namespace never inherits secrets left by an earlier namespace of the same name (for example after a give-up below). Until it succeeds nothing granting Vault access is created: `VaultReady=False` (reason `PurgePending`) and a backoff requeue; the namespace itself is already usable (`Ready=True`). Success is recorded in `status.vaultPurgedAt`, so later reconciles, or a tenant deleting its VaultAuth, never purge live data.

**Purge on deletion.** A TeamNamespace then carries a second finalizer, `idp.ithaca-idp.com/vault-purge`. On deletion the operator first deletes the namespace (as before, it does not wait for Vault), then logs in (`POST /v1/auth/<mount>/login`, role `VAULT_PURGE_ROLE`, JWT read fresh from `VAULT_TOKEN_PATH` each time), walks `GET /v1/kv/metadata/<ns>?list=true` recursively and sends `DELETE /v1/kv/metadata/<ns>/<key>` per key (all versions and metadata), then revokes its token. A 404 (no data) is success. This stops a re-created namespace with the same name from inheriting old secrets: the old TeamNamespace, and so the name, is held until the purge is done.

- Guards: the namespace name must be a DNS-1123 label and not reserved before it goes into a path (checked inside the purger too); on deletion the purge is skipped when a namespace of that name exists but is not managed by the TeamNamespace. Keys returned by Vault that are empty, `.`, `..` or contain `/` are skipped and reported; everything else is escaped per segment (`url.PathEscape`). Subtrees deeper than 32 levels are skipped and reported; siblings are still purged and the pass then fails so it retries. Keys are capped at 5000 per pass (later passes continue); 10 s per request, 2 min per pass; redirects are not followed; tokens, the JWT and response bodies are never logged or put into status.
- Vault down or failing: the finalizer stays, condition `VaultPurgePending=True` (reason `VaultPurgeRetrying`) shows the error and deadline, and the operator retries with backoff (10 s growing to 5 min).
- Reserved names include `vault` and `vault-*` (the platform's Vault namespaces), because the purge role may delete anywhere under `kv/metadata/`.
- Give-up: once `VAULT_PURGE_GIVE_UP_AFTER` has passed since the deletion timestamp, the operator logs an error and removes the finalizer anyway, so a destroyed Vault cannot block deletion forever. Data left in that Vault is orphaned (in this lab, Vault is rebuilt empty on `make up`). A same-name namespace created after a give-up is still protected by the purge before access.
- Force removal now (admin, e.g. Vault is gone for good). The `test` op makes the patch fail unless index 0 is the Vault finalizer (the namespace finalizer is normally gone by then; if not, check `.metadata.finalizers` and adjust the index):
  `kubectl patch tns <name> --type=json -p '[{"op":"test","path":"/metadata/finalizers/0","value":"idp.ithaca-idp.com/vault-purge"},{"op":"remove","path":"/metadata/finalizers/0"}]'`.
  Purge `kv/<name>/` by hand later if Vault returns.
- If the integration is turned off while a TeamNamespace with the Vault finalizer is deleting, the finalizer is dropped without a purge.

**Deployment needs** (idp-gitops `workloads/idp-operator`): the `VAULT_*` env above (at least `VAULT_ENABLED=true`), a projected `serviceAccountToken` volume (`audience: vault`, `path: token`, `expirationSeconds: 600`) mounted read-only at `/var/run/secrets/vault`, and egress to Vault on 8200. No extra RBAC: the projected token avoids `serviceaccounts/token` rights.

## ManagedSecret (portal Secrets page)

Namespaced `ManagedSecret` (`idp.ithaca-idp.com/v1alpha1`, short name `msec`), written by portal-api only. `metadata.name` is the secret name: DNS-1123 label, at most 40 chars, not `vault-reader` or `default` and not starting with `default-token`, `vault-reader-token`, `sh-helm`, `kube-`, `istio-` or `idp-` (legacy token Secrets, Helm release Secrets, platform and operator names). `spec` has no fields (unknown fields are pruned); no secret value ever passes through it.

Only with `VAULT_ENABLED=true` (otherwise the controller is not started and ManagedSecrets get no status). In a namespace that exists, carries the operator labels, has a live TeamNamespace of the same name and a VaultAuth `vault-reader`, the operator creates, controlled by the ManagedSecret:

```yaml
apiVersion: secrets.hashicorp.com/v1beta1
kind: VaultStaticSecret
metadata: {name: <name>, namespace: <ns>, labels: {app.kubernetes.io/managed-by: idp-operator, idp.ithaca-idp.com/managed-secret: <name>}}
spec:
  vaultAuthRef: vault-reader
  mount: kv
  type: kv-v2
  path: <ns>/<name>
  refreshAfter: 60s
  hmacSecretData: true            # the CRD default, written so drift checks compare equal
  destination: {name: <name>, create: true, overwrite: false}
```

The spec is overwritten on every reconcile, but only on a VaultStaticSecret controlled by this ManagedSecret (same UID). Conditions (`status.observedGeneration` too):

- `Synced`: `True` means the VaultStaticSecret exists with this spec, **not** that the Secret was written. `False` reasons: `NamespaceNotManaged` (no requeue), `VaultAuthMissing` (15 s), `NameConflict` (a VaultStaticSecret of that name exists that this ManagedSecret does not control, e.g. made by hand; it is never adopted or changed; 5 min), `PreviousTerminating` (one from an earlier ManagedSecret of the same name, or a terminating one behind VSO's finalizer, is still there; 5 s), `SyncError`.
- `Delivered`: mirrors VSO's `SecretSynced` condition on the VaultStaticSecret for its current generation (`True`/`Delivered`, `False`/`DeliveryFailed`), otherwise `Unknown`/`DeliveryPending`. VSO messages are not copied. VSO 1.6.2 does not write status when it refuses to write the Secret (a Secret `<name>` it did not create exists, `overwrite: false`); it only emits an event, so that case stays `Unknown`. The operator has no Secret read access to detect it.

Limits: a ManagedSecret being deleted (deletionTimestamp set, waiting for garbage collection) still exists for a portal `GET`; the admission policy cannot look up the owning ManagedSecret, so an ownerReference with a made-up UID passes it and is only cleaned up by the garbage collector.

**Delete.** The portal deletes the ManagedSecret (default background propagation; do not use `Foreground`, which would wait on VSO's finalizer). The garbage collector then deletes the VaultStaticSecret (VSO drops its finalizer `vaultstaticsecret.secrets.hashicorp.com/finalizer`), and then the Kubernetes Secret `<name>`, which VSO 1.6.2 creates with an ownerReference to the VaultStaticSecret (`helpers/secrets.go`, `create: true`). The KV data in Vault is **not** deleted: the portal removes `kv/<ns>/<name>` itself; the operator only purges `kv/<ns>/` when the TeamNamespace is deleted. With `overwrite: false`, VSO refuses to take over an existing Secret `<name>` it did not create (the VaultStaticSecret then reports an error), so a ManagedSecret cannot replace or later garbage-collect a foreign Secret.

## WorkloadIdentity (Order #03)

Namespaced `WorkloadIdentity` (short name `wid`), written by portal-api (PR 2). `metadata.name` is the workload name, with the same rules as a ManagedSecret name; `spec` has no fields. Only in a managed namespace (as for ManagedSecrets; otherwise `NamespaceNotManaged`), the operator creates, all named `<name>`, labelled `idp.ithaca-idp.com/workload=<name>` and controlled by the WorkloadIdentity (never adopting an existing object: `NameConflict`, or `PreviousTerminating` while one from an earlier WorkloadIdentity of that name waits for garbage collection):

| Object | Notes |
|---|---|
| AuthorizationPolicy (`security.istio.io/v1`) | created first; `ALLOW` for pods labelled `idp.ithaca-idp.com/workload=<name>` from `namespaces: [<ns>]` and `principals: [<trust domain>/ns/istio-ingress/sa/ingress-istio]` (the gateway's ServiceAccount, `<gateway>-<class>`). L4 only, because ambient without a waypoint is enforced by ztunnel. A ServiceAccount cannot be a policy target in Istio 1.27 (`targetRefs` takes Gateway, GatewayClass, Service, ServiceEntry and is enforced by waypoints), so pods are selected by label: **workload pods must carry `idp.ithaca-idp.com/workload=<name>`**, otherwise only the namespace NetworkPolicies apply to them. |
| ServiceAccount | `automountServiceAccountToken: false`; create-only (the operator has no `update` on it). Istio issues `spiffe://<trust domain>/ns/<ns>/sa/<name>` to pods running as it. |
| HTTPRoute (`gateway.networking.k8s.io/v1`) | parent `istio-ingress/ingress`, section `https`; hostname `<name>.<ns>.<suffix>`; one rule (PathPrefix `/`) with no `backendRefs` or filters. The Gateway API spec answers a matching request with 500 when a rule has no valid backend and no responding filter, so the route only reserves the hostname (Istio is expected to send a direct 500; not yet observed on the cluster). |
| VaultAuth (only with `VAULT_ENABLED=true`) | as `vault-reader`'s, but `role: workload-reader`, `serviceAccount: <name>`. The Vault role (policy `kv/data/<ns>/<sa name>/*`) is configured in idp-gitops; the operator does not write to Vault. |

Status: `spiffeID`, `hostname`, `observedGeneration`, condition `Synced` (`True`/`Synced`; `False`: `NamespaceNotManaged`, `NameConflict`, `PreviousTerminating`, `SyncError`). Specs are reverted on drift, except the ServiceAccount; annotations on the children are dropped (the policy refuses any, e.g. `istio.io/dry-run`). Namespace and TeamNamespace events re-queue the WorkloadIdentities in that namespace, so one created early syncs once the namespace is managed. Deleting the WorkloadIdentity removes the children by garbage collection. With `VAULT_ENABLED=true` it carries the finalizer `idp.ithaca-idp.com/workload-vault-purge`: on deletion the operator purges `kv/<ns>/<name>/` (only that directory, not a leaf key `kv/<ns>/<name>`) with the TeamNamespace purge's login, retry, `VaultPurgePending` condition and `VAULT_PURGE_GIVE_UP_AFTER`; it is skipped when the namespace exists without the operator labels.

| Flag (env) | Default | Meaning |
|---|---|---|
| `--gateway-namespace` (`GATEWAY_NAMESPACE`) | `istio-ingress` | namespace of the ingress Gateway pods admitted by `idp-allow-ingress-gateway`; any other value is refused at start any other value is refused at start (the admission policy pins the default) |
| `--gateway-name` (`GATEWAY_NAME`) | `ingress` | Gateway name, matched on the pod label `gateway.networking.k8s.io/gateway-name` any other value is refused at start (the admission policy pins the default) |
| `--trust-domain` (`TRUST_DOMAIN`) | `cluster.local` | Istio trust domain for `spiffeID` and the gateway principal |
| `--workload-hostname-suffix` (`WORKLOAD_HOSTNAME_SUFFIX`) | `apps.ithaca-idp.com` | hostname suffix; the admission policy pins `.apps.ithaca-idp.com`, change both together |

At start the operator checks (RESTMapper) that the HTTPRoute and AuthorizationPolicy CRDs, and with Vault on the VaultAuth CRD, are served; if one is missing it logs `WorkloadIdentity controller disabled` and runs without that controller (restart it after installing the CRDs).

## Develop

```sh
make test     # controller-gen (deepcopy, CRD, RBAC), fetch envtest binaries into ./bin, go test
make check    # gofmt, go vet, test, govulncheck
```

Tool versions are pinned in the `Makefile` and run with `go run`. Generated files (`api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases`, `config/rbac/role.yaml`) are committed. After `make envtest`, plain `go test ./...` also finds the binaries; without them the envtest tests skip.

## RBAC rationale

- The child objects live in namespaces created at runtime, so `create` on resourcequotas, limitranges, networkpolicies and rolebindings is cluster-wide. `update` (and `delete` on rolebindings) is limited by `resourceNames` to the operator's fixed child names.
- Creating a RoleBinding to `admin` requires holding every permission in `admin` or `bind` on it. The operator gets `bind` on `clusterroles` with `resourceNames: [admin]` only; no `escalate`, no wildcards.
- ServiceAccounts: `create` cluster-wide, `update`/`delete` pinned to `vault-reader`. VaultAuths: `create`/`update` cluster-wide (WorkloadIdentity VaultAuths follow the workload names), `delete` pinned to `vault-reader`. No `serviceaccounts/token`. The rules exist even with Vault off; the admission policy confines them.
- HTTPRoutes and AuthorizationPolicies: `get`/`list`/`watch`/`create`/`update` cluster-wide, no `delete`. WorkloadIdentities: read, `update` (the Vault purge finalizer), `status` update, and `finalizers` update for `blockOwnerDeletion`.
- VaultStaticSecrets: `get`/`list`/`watch`/`create`/`update` cluster-wide (names follow the ManagedSecrets, so `resourceNames` cannot pin them), no `delete` (garbage collection removes them). ManagedSecrets: read, `status` update, and `finalizers` update for `blockOwnerDeletion`. A VaultStaticSecret that exists without the owner reference cannot be adopted on clusters with `OwnerReferencesPermissionEnforcement` (changing ownerReferences needs `delete`); it stays `SyncError`.
- Namespaces need `create`, `update` and `delete` cluster-wide; RBAC cannot scope them by label, so the label check in code is the guard.
- `tokenreviews`/`subjectaccessreviews` `create` protect the HTTPS metrics endpoint; leases and events are a namespaced Role in `idp-system` for leader election.

## Admission policy

`config/policy/operator-scope.yaml` holds a `ValidatingAdmissionPolicy` and its binding (`idp-operator-scope`, `failurePolicy: Fail`, action `Deny`). `matchConditions` limit it to `system:serviceaccount:idp-system:idp-operator`, so other users are not evaluated. Deploy it before (or with) the operator. For that user it enforces:

- Namespaces: `CREATE` only with `managed-by=idp-operator` and `team-namespace=<name>`, and not for a reserved name; `UPDATE`/`DELETE` only if the stored object (`oldObject`) already carries both labels, and an `UPDATE` cannot change or drop them. PSS `enforce` must stay `baseline` or `restricted`. Subresources are included.
- ResourceQuotas, LimitRanges, NetworkPolicies, RoleBindings, ServiceAccounts, VaultAuths: only in namespaces carrying both labels, and only under the fixed child names (ServiceAccount and VaultAuth: `vault-reader`). `idp-allow-ingress-gateway` is also pinned to its exact spec: all pods, Ingress, one rule with one peer holding both the `istio-ingress` namespace selector and the `gateway-name=ingress` pod selector, only TCP 15008 (extra or empty peers, ipBlocks, other selectors and egress are denied). Subresources such as `serviceaccounts/token` fall under the same rule.
- VaultAuths: the spec must be exactly what the controller writes (`method`/`mount` kubernetes, `vaultConnectionRef: vault-secrets-operator/idp-vault`, role `ns-reader`, ServiceAccount `vault-reader`, audiences `[vault]`, `tokenExpirationSeconds: 600`) and none of `allowedNamespaces`, `headers`, `params`, `namespace`, `appRole`, `aws`, `gcp`, `jwt`, `storageEncryption`, `vaultAuthGlobalRef` (the VSO 1.6.2 field set), so the operator cannot point a VaultAuth at the purge role, another ServiceAccount, connection or Vault namespace.
- VaultStaticSecrets: only in managed namespaces, with exactly one ownerReference, `controller: true`, to a `ManagedSecret` of the same name, and on `UPDATE` the stored object must already have it, so a hand-made VaultStaticSecret cannot be adopted (the controller also leaves it alone with `NameConflict`; on clusters with `OwnerReferencesPermissionEnforcement` adding the ownerReference is refused earlier for lack of `delete`). The policy cannot look the ManagedSecret up; a made-up owner UID is removed by the garbage collector, and exactly the spec above: `vaultAuthRef: vault-reader`, `mount: kv`, `type: kv-v2`, `path: <request namespace>/<name>`, `refreshAfter: 60s`, `hmacSecretData` true, destination name `<name>` with `create: true` and `overwrite: false`, and none of `namespace`, `version`, `rolloutRestartTargets`, `syncConfig`, `destination.type/labels/annotations/transformation` (the VSO 1.6.2 field set).
- WorkloadIdentity children (ServiceAccounts and VaultAuths other than `vault-reader`, all HTTPRoutes and AuthorizationPolicies): only in managed namespaces, with exactly one controller ownerReference to a `WorkloadIdentity` of the same name (on `UPDATE` already on the stored object), under a valid WorkloadIdentity name, with no annotations, and with the fixed shape: ServiceAccount without token automount, `secrets` or `imagePullSecrets`; VaultAuth role `workload-reader` as ServiceAccount `<name>` (and `vault-reader` keeps role `ns-reader`); HTTPRoute with one parentRef `istio-ingress/ingress` section `https`, exactly the hostname `<name>.<ns>.apps.ithaca-idp.com` and no `backendRefs` or `filters`; AuthorizationPolicy `ALLOW`, selector exactly `idp.ithaca-idp.com/workload=<name>`, one rule with exactly the two sources above (principal checked as `<trust domain>/ns/istio-ingress/sa/ingress-istio`), no `targetRefs`, `provider`, `to`, `when` or other source fields.
- RoleBindings: roleRef `ClusterRole admin`, exactly one `User` subject that contains `@` and does not start with `system:` (no groups, ServiceAccounts or `system:serviceaccount:...` users).

`config/policy/portal-scope.yaml` (`idp-portal-scope`, same style) evaluates only `system:serviceaccount:idp-system:portal-api` on `managedsecrets` and its subresources: `CREATE`/`DELETE` only in namespaces carrying both operator labels, never `UPDATE` or a subresource write, and no `finalizers` or `ownerReferences` on create. Its RBAC (get, create, delete on `managedsecrets`, no list/watch/update/patch) lives in idp-gitops `workloads/portal/portal-rbac.yaml`.

What it does not protect:

- Anyone with cluster-admin, or with write access to `validatingadmissionpolicies`/`validatingadmissionpolicybindings`, can delete or edit the policy. Renaming the ServiceAccount or its namespace silently disables it.
- Other users labelling a namespace to hand it to the operator (see Known limits).
- Inside managed namespaces the operator can still delete them, rewrite quotas and limits, and grant `admin` to any email-looking user. It can also create labelled namespaces under any non-reserved name (squatting a future team name).
- `namespaceObject` comes from the apiserver's informer cache, so for a short window after an admin removes the labels the operator may still write children in that namespace.
- Writes to other kinds are left to RBAC (the policy is not an allowlist of everything the operator can touch).

envtest installs test-only stubs of the VSO VaultAuth and VaultStaticSecret CRDs (`test/crds/`, typed fields only, so misspelt fields are pruned and fail the tests; the real CRD comes from the VSO chart) and unmodified upstream copies of the Gateway API v1.3.0 HTTPRoute and Istio 1.27.1 AuthorizationPolicy CRDs. `internal/controller/policy_envtest_test.go` installs `config/rbac` and `config/policy` in envtest (RBAC on), impersonates the ServiceAccount, runs the reconciler end to end, checks the denials with server-side dry-run, and removes the binding to show those denials come from the policy and not RBAC.

## Known limits

- RBAC alone would let a compromised operator create objects (including `admin` RoleBindings) in any namespace and delete any namespace; the admission policy below closes this.
- Anyone (other than the operator) who can label namespaces can make an existing namespace adoptable by adding the two operator labels. The policy cannot stop this; restrict namespace `update`/`patch` to platform admins.
- The informer cache watches all namespaces, quotas, limit ranges, network policies and role bindings cluster-wide.
- The finalizer is removed once the namespace delete is accepted, not when it has finished terminating; a CR recreated meanwhile waits in `Pending`.
- The ingress Gateway is admitted by `idp-allow-ingress-gateway`; waypoints are not (none exist yet, so a later change must add an allow rule for them). Reachability through the real gateway is not covered by the envtests.
- Grafana folder is not implemented yet. Vault per-namespace access is behind `VAULT_ENABLED` (above); with it on, the cache also watches ServiceAccounts, VaultAuths, ManagedSecrets and VaultStaticSecrets cluster-wide, and the VSO CRDs (VaultAuth, VaultStaticSecret) and the ManagedSecret CRD must exist before the operator starts.
