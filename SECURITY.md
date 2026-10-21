# Security policy

## Reporting a vulnerability

Please use GitHub's private advisory form: **Security -> Report a vulnerability**. Do not open a public issue. Expect an acknowledgement within 7 days.

## Scope and posture

This repo provisions lab infrastructure. Design commitments:

- No service-account keys. CI uses Workload Identity Federation; pull requests get a read-only identity, only `main` can apply.
- Secret scanning, push protection and Dependabot security updates are enabled.
- Workflows are pinned to commit SHAs, run with least-privilege `permissions`, and are linted by zizmor and CodeQL.
