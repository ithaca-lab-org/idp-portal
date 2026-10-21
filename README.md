# idp-portal

Self-service portal for the ithaca IDP. Today: `api/`, a Go service with OIDC login against Keycloak realm `ithaca` (Authorization Code + PKCE, signed-cookie session). `web/`, `operator/` and `notifier/` follow in Phase 1.

| Endpoint | Purpose |
|---|---|
| `GET /auth/login` | Redirects to Keycloak with state, nonce and a PKCE challenge |
| `GET /auth/callback` | Verifies state, exchanges the code, verifies the ID token and nonce, sets the session cookie |
| `GET /auth/me` | Returns the logged-in user (`sub`, `username`, `email`, `groups`) or 401 |
| `POST /auth/logout` | Same-origin only. Clears the session cookie and returns `end_session_url` so the browser also ends the Keycloak SSO session |
| `GET /healthz` | Liveness |

Environment: `OIDC_ISSUER` (for example `https://<host>/auth/realms/ithaca`), `OIDC_CLIENT_ID` (`idp-portal`), `OIDC_REDIRECT_URL`, `SESSION_KEY` (base64 of 32+ random bytes, `openssl rand -base64 32`, from a Secret), optional `LISTEN_ADDR`, `COOKIE_INSECURE=true` for plain-HTTP local runs.

`make check` runs vet and tests.

## Security notes

- Cookies are stateless HMAC-signed values, bound to their purpose (a login cookie cannot be replayed as a session cookie), `HttpOnly`, `SameSite=Lax`, and `__Host-` prefixed when `Secure`, so sibling subdomains cannot set them.
- Sessions last 15 minutes and groups are read only at login, so group changes apply at the next login. A server-side session store (needed anyway to call Kubernetes and Vault for the user) will add revocation.
- `email` is not verified by this service and must never be used for authorisation.
