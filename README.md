# idp-portal

Self-service portal for the ithaca IDP. Today: `api/`, a Go service with OIDC login against Keycloak realm `ithaca` (Authorization Code + PKCE, signed-cookie session). `web/`, `operator/` and `notifier/` follow in Phase 1.

| Endpoint | Purpose |
|---|---|
| `GET /auth/login` | Redirects to Keycloak with state, nonce and a PKCE challenge |
| `GET /auth/callback` | Verifies state, exchanges the code, verifies the ID token and nonce, sets the session cookie |
| `GET /auth/me` | Returns the logged-in user (`sub`, `username`, `email`, `groups`) or 401 |
| `POST /auth/logout` | Same-origin only. Clears the session cookie and returns `end_session_url` so the browser also ends the Keycloak SSO session |
| `GET /healthz` | Liveness |

Container image: `us-central1-docker.pkg.dev/project-324502ff-9928-4b17-a89/idp/portal-api:<git sha>`, built by `.github/workflows/image.yml` (distroless, non-root, digest-pinned bases, Trivy gate, provenance and SBOM attestations). Pull requests build and scan only; pushes to `main` push through Workload Identity Federation.

Environment: `OIDC_ISSUER` (for example `https://<host>/auth/realms/ithaca`), `OIDC_CLIENT_ID` (`idp-portal`), `OIDC_REDIRECT_URL`, `SESSION_KEY` (base64 of 32+ random bytes, `openssl rand -base64 32`, from a Secret), optional `OIDC_EXTRA_CA_FILE` (extra trusted CA for the issuer, only while the platform serves Let's Encrypt staging certificates), optional `LISTEN_ADDR`, `COOKIE_INSECURE=true` for plain-HTTP local runs.

`make check` runs vet and tests.

## Security notes

- Cookies are stateless HMAC-signed values, bound to their purpose (a login cookie cannot be replayed as a session cookie), `HttpOnly`, `SameSite=Lax`, and `__Host-` prefixed when `Secure`, so sibling subdomains cannot set them.
- Sessions last 15 minutes and groups are read only at login, so group changes apply at the next login. A server-side session store (needed anyway to call Kubernetes and Vault for the user) will add revocation.
- `email` is not verified by this service and must never be used for authorisation.

## Order API

All routes need a signed-in session (401 JSON otherwise). The caller's email and groups come from the session only.

| Endpoint | Purpose |
|---|---|
| `GET /api/business-units` | The caller's groups: the business units they may order for |
| `POST /api/orders/namespaces` | `{"name","businessUnit","size"}` -> 202 with the order (`PENDING`) |
| `GET /api/orders` | The caller's orders, newest first (`?limit=`, default 50, max 100) |
| `GET /api/orders/{id}` | One of the caller's orders (someone else's is a 404, so ids cannot be enumerated) |

Rules enforced on `POST`:
- the business unit must be exactly one of the caller's `groups` (403 otherwise); the owner is always the session email, and unknown JSON fields (for example a forged `owner`) are rejected;
- the name must be a lowercase DNS label of at most 40 characters and not reserved (`default`, `kube-*`, `istio-*`, `gke-*`, `argocd`, `cert-manager`, `tailscale`, `identity`, `idp-system`, `echo`); a test fails if this list drifts from the operator's;
- size is `small` or `medium`; at most 10 active orders per business unit and 5 per user (`ORDER_CAP_PER_BU`, `ORDER_CAP_PER_USER`). The count and the insert happen under one lock, so a burst cannot exceed a cap (single replica; more replicas need a database-side reservation). Users are identified by their Keycloak subject, not their email, which is editable in the directory and unverified; the email is lowercased and used only as the namespace owner. At most 10 order attempts per user per minute (429 with `Retry-After`);
- JSON only (`Content-Type: application/json`), 8 KB limit, same-origin only (cross-site requests get 403).

Flow: the order is stored as `PENDING` (number `ORD-<year>-<counter>`, one active order per name), then a `TeamNamespace` is created with the order id. A poller (every 5 s, `PENDING` orders only) and reads move the order to `COMPLETE` or `FAILED` from the resource's status. A resource that belongs to a different order, or never appears within 10 minutes, fails the order. A `COMPLETE` order whose namespace resource is later removed becomes `RELEASED` (checked every minute), which frees the name and the caps.

Environment: `MONGO_URI` (secret, required; the server refuses to start without it), `MONGO_DB` (default `idp`), `ORDER_CAP_PER_BU`, `ORDER_CAP_PER_USER`. The pod's ServiceAccount needs `get`, `list`, `watch` and `create` on `teamnamespaces.idp.ithaca-idp.com` and nothing else.

### Secrets page (off unless `SECRETS_ENABLED=true`; off, the routes answer 404 and the page is hidden)

The portal is a write-only Vault client: it writes new KV v2 versions and deletes secrets, it never reads a value back. A user may manage the secrets of namespace `<ns>` only if a `COMPLETE` namespace order for `<ns>` belongs to one of the user's `groups`; anything else is a 404, so existence does not leak.

- `GET /api/namespaces` (also returns the reserved secret names and prefixes, so the page validates like the API), `GET /api/namespaces/{ns}/secrets` (metadata: name, key names, version, updatedBy, updatedAt);
- `PUT /api/namespaces/{ns}/secrets/{name}` with `{"data":{"KEY":"value"}}` replaces the whole secret and answers `{"name","keys","version"}`; with `?create=true` (the page's "New secret") it answers 409 if the secret exists. Name: DNS label of at most 40, not `vault-reader` or `default`, not starting with `default-token`, `vault-reader-token`, `sh-helm`, `kube-`, `istio-` or `idp-` (identical to the ManagedSecret CRD; a test fails if they drift); 1 to 20 keys matching `^[A-Za-z_][A-Za-z0-9_]{0,63}$`; values 1 to 8192 bytes, 64 KB in total; JSON only, 128 KB body limit, unknown fields rejected, same-origin only;
- `DELETE /api/namespaces/{ns}/secrets/{name}` (204, idempotent);
- 20 writes and 120 reads per user per minute.

PUT first creates the `ManagedSecret` `<name>` in `<ns>` (empty spec; an existing one is fine; a name the CRD rejects is a 400, so a bad name never reaches Vault), then writes `kv/data/<ns>/<name>`, then stores the metadata in the Mongo collection `secrets`. Until the write lands, a resource without a value only shows a sync error. If Vault fails the answer is 502 and the metadata is untouched; repeating the request is idempotent. Metadata carries the id of the namespace's `COMPLETE` order: when a namespace is released and ordered again (possibly by another business unit), the previous team's metadata is never listed and is purged on the next list or write. DELETE removes the resource, then `kv/metadata/<ns>/<name>` (all versions), then the metadata. Audit events `secret.changed` / `secret.deleted` (namespace, name, actor, key names, version) go to the order-events topic. Values never reach MongoDB, the resource, logs, errors or events.

Vault calls (the `portal-secrets` policy needs exactly these, and no read or list anywhere): `POST auth/<mount>/login`, `PUT <kv>/data/<ns>/<name>` (create, update on `<kv>/data/*`), `DELETE <kv>/metadata/<ns>/<name>` (delete on `<kv>/metadata/*`).

Known limits: group membership comes from the session, so a group change applies after the next login (sessions last 15 minutes); writes to one secret are serialized by an in-process lock, which is correct for the single portal replica only.

Environment: `SECRETS_ENABLED`, `VAULT_ADDR` (default `http://vault.vault.svc.cluster.local:8200`), `VAULT_SECRETS_ROLE` (`portal-secrets`), `VAULT_AUTH_MOUNT` (`kubernetes`), `VAULT_KV_MOUNT` (`kv`), `VAULT_TOKEN_PATH` (`/var/run/secrets/vault/token`, a projected ServiceAccount token with audience `vault`, re-read at every login). The ServiceAccount additionally needs `create`, `get` and `delete` on `managedsecrets.idp.ithaca-idp.com`.
