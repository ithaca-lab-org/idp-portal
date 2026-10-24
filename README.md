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
