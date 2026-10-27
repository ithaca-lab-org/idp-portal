# idp-portal rules

Part of the ithaca IDP. Architecture and decisions live in `../IDP_NOTES.md` (sections 1, 3, 10, 11).

- `api/` Go service (portal-api). `web/` (React + TypeScript, built into the API image and served via go:embed; `make web`, `make web-check`), `operator/` (kubebuilder) and `notifier/` (Pub/Sub order events to email; sending is off unless `NOTIFIER_SEND_EMAIL=true`) are separate Go modules.
- The portal never touches Kubernetes directly: it writes Custom Resources and the operator reconciles them.
- Secret values never go into MongoDB, CRDs, Git or logs. Session and OIDC secrets come from the environment (Kubernetes Secrets), never from files in the repo.
- Auth is OIDC against Keycloak realm `ithaca` (public client `idp-portal`, Authorization Code + PKCE). Security-relevant changes (`api/internal/auth`, `api/internal/session`) get an Opus review.
- Run `make check` (vet + test) before finishing. Conventional commits, PRs to `main`. The repo is public: no secrets or account IDs.
- Never run `kubectl apply/delete` or `terraform apply`. Argo CD deploys; the user applies.
