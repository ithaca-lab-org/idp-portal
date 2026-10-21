# Contributing

1. Branch from `main`; never push to `main` directly.
2. `pre-commit install --install-hooks && pre-commit install --hook-type commit-msg`
3. Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat(gke): add spot pool`). The PR title is checked too, because squash-merge uses it as the commit message.
4. `make check` must pass (gofmt, `go vet`, `go test`). Auth and session code gets a security review before merge.
5. Record significant decisions as an ADR in `docs/adr/`.

Releases are automatic: merging the release-please PR tags a version and publishes the changelog. `CLAUDE.md` holds the rules AI agents follow in this repo.
