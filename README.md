# idp-portal

Self-service portal for the ithaca IDP. Today: `api/`, a Go service with OIDC login against Keycloak realm `ithaca` (Authorization Code + PKCE, signed-cookie session). `web/`, `operator/` and `notifier/` follow in Phase 1.

| Endpoint | Purpose |
|---|---|
| `GET /auth/login` | Redirects to Keycloak with state, nonce and a PKCE challenge |
| `GET /auth/callback` | Verifies state, exchanges the code, verifies the ID token and nonce, sets the session cookie |
| `GET /auth/me` | Returns the logged-in user (`sub`, `username`, `email`, `groups`) or 401 |
| `POST /auth/logout` | Clears the session cookie |
| `GET /healthz` | Liveness |

Environment: `OIDC_ISSUER` (for example `https://<host>/auth/realms/ithaca`), `OIDC_CLIENT_ID` (`idp-portal`), `OIDC_REDIRECT_URL`, `SESSION_KEY` (32+ random bytes, from a Secret), optional `LISTEN_ADDR`, `COOKIE_INSECURE=true` for plain-HTTP local runs.

`make check` runs vet and tests.
