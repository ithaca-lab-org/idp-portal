# Changelog

## 0.1.0 (2026-10-29)


### Features

* **api:** landing page at / instead of a bare 404 ([#4](https://github.com/ithaca-lab-org/idp-portal/issues/4)) ([39f44c8](https://github.com/ithaca-lab-org/idp-portal/commit/39f44c81692079ae17ef2fd6cff7a79bdc23f672))
* **api:** namespace order API with MongoDB order history ([#9](https://github.com/ithaca-lab-org/idp-portal/issues/9)) ([1c2c3b9](https://github.com/ithaca-lab-org/idp-portal/commit/1c2c3b92554672a136e885d209b13536ccd39e3b))
* **api:** OIDC login skeleton with signed-cookie sessions ([b2e0121](https://github.com/ithaca-lab-org/idp-portal/commit/b2e01214f2e3669469caf621cadbad0529500ce8))
* **api:** publish order events to Pub/Sub ([#16](https://github.com/ithaca-lab-org/idp-portal/issues/16)) ([9bace4d](https://github.com/ithaca-lab-org/idp-portal/commit/9bace4db9df9a5b084c4cdb033297ed4390c24c8))
* container image, build workflow and staging CA trust ([#3](https://github.com/ithaca-lab-org/idp-portal/issues/3)) ([cccd5f7](https://github.com/ithaca-lab-org/idp-portal/commit/cccd5f724319c5155ef9ecab8597441666b03f9e))
* harden portal auth per Opus review and add repo scaffolding ([9c2a71f](https://github.com/ithaca-lab-org/idp-portal/commit/9c2a71fae602a7982cfec99405a47faf1ac7c475))
* **notifier:** Pub/Sub order events to email, sending off by default ([#17](https://github.com/ithaca-lab-org/idp-portal/issues/17)) ([b789daa](https://github.com/ithaca-lab-org/idp-portal/commit/b789daaab66085c7f77a7c4cb45c19c5d4d82929))
* **operator:** admission policy confining the operator ServiceAccount ([#8](https://github.com/ithaca-lab-org/idp-portal/issues/8)) ([fc2c9f3](https://github.com/ithaca-lab-org/idp-portal/commit/fc2c9f3c6c1e0279ed142c97f9d660dd7690773f))
* **operator:** container image built and pushed by the image workflow ([#7](https://github.com/ithaca-lab-org/idp-portal/issues/7)) ([8f4cbaa](https://github.com/ithaca-lab-org/idp-portal/commit/8f4cbaa00cdc3ba9584d8152d65f0dec0bf1e567))
* **operator:** ManagedSecret CRD and controller (pinned VaultStaticSecret) ([#22](https://github.com/ithaca-lab-org/idp-portal/issues/22)) ([62b6839](https://github.com/ithaca-lab-org/idp-portal/commit/62b68394bd7c9b2a4e44730558e6bd186019ee94))
* **operator:** per-namespace Vault access and KV purge, behind VAULT_ENABLED ([#19](https://github.com/ithaca-lab-org/idp-portal/issues/19)) ([e9afff1](https://github.com/ithaca-lab-org/idp-portal/commit/e9afff1deb9c038553cdc4504b8d06d48d33c4db))
* **operator:** TeamNamespace CRD and reconciler ([#5](https://github.com/ithaca-lab-org/idp-portal/issues/5)) ([aefba1e](https://github.com/ithaca-lab-org/idp-portal/commit/aefba1e7a60710a410aa1fefe1826ff76990db8a))
* **portal:** write-only Secrets page behind SECRETS_ENABLED ([#21](https://github.com/ithaca-lab-org/idp-portal/issues/21)) ([a88e09f](https://github.com/ithaca-lab-org/idp-portal/commit/a88e09fd8754104de3d9366d2fa162f4ea894d62))
* **web:** React UI served by portal-api ([#10](https://github.com/ithaca-lab-org/idp-portal/issues/10)) ([d9c2049](https://github.com/ithaca-lab-org/idp-portal/commit/d9c2049aa2add05d8897a4712af2eaf0b48e03db))


### Bug Fixes

* go-jose 4.1.6 and MongoDB 7.0 for CI (8.x refuses to start on Linux 6.19+ runners) ([#20](https://github.com/ithaca-lab-org/idp-portal/issues/20)) ([e4351ec](https://github.com/ithaca-lab-org/idp-portal/commit/e4351ec64b79ed3543d82fd71afb83d7683f18e0))
