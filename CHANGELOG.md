# Changelog

## 0.1.0 (2026-11-14)


### Features

* **api:** landing page at / instead of a bare 404 ([#4](https://github.com/ithaca-lab-org/idp-portal/issues/4)) ([39f44c8](https://github.com/ithaca-lab-org/idp-portal/commit/39f44c81692079ae17ef2fd6cff7a79bdc23f672))
* **api:** namespace order API with MongoDB order history ([#9](https://github.com/ithaca-lab-org/idp-portal/issues/9)) ([1c2c3b9](https://github.com/ithaca-lab-org/idp-portal/commit/1c2c3b92554672a136e885d209b13536ccd39e3b))
* **api:** OIDC login skeleton with signed-cookie sessions ([b2e0121](https://github.com/ithaca-lab-org/idp-portal/commit/b2e01214f2e3669469caf621cadbad0529500ce8))
* **api:** publish order events to Pub/Sub ([#16](https://github.com/ithaca-lab-org/idp-portal/issues/16)) ([9bace4d](https://github.com/ithaca-lab-org/idp-portal/commit/9bace4db9df9a5b084c4cdb033297ed4390c24c8))
* **broker:** BucketClaim and ResourceGrant CRDs and the resource broker (ADR 0010 step 2) ([#33](https://github.com/ithaca-lab-org/idp-portal/issues/33)) ([d434f74](https://github.com/ithaca-lab-org/idp-portal/commit/d434f74da0f4621f5bb8987968394945bb9887c0))
* container image, build workflow and staging CA trust ([#3](https://github.com/ithaca-lab-org/idp-portal/issues/3)) ([cccd5f7](https://github.com/ithaca-lab-org/idp-portal/commit/cccd5f724319c5155ef9ecab8597441666b03f9e))
* harden portal auth per Opus review and add repo scaffolding ([9c2a71f](https://github.com/ithaca-lab-org/idp-portal/commit/9c2a71fae602a7982cfec99405a47faf1ac7c475))
* **notifier:** Pub/Sub order events to email, sending off by default ([#17](https://github.com/ithaca-lab-org/idp-portal/issues/17)) ([b789daa](https://github.com/ithaca-lab-org/idp-portal/commit/b789daaab66085c7f77a7c4cb45c19c5d4d82929))
* **operator:** admission policy confining the operator ServiceAccount ([#8](https://github.com/ithaca-lab-org/idp-portal/issues/8)) ([fc2c9f3](https://github.com/ithaca-lab-org/idp-portal/commit/fc2c9f3c6c1e0279ed142c97f9d660dd7690773f))
* **operator:** allow ingress-gateway HBONE traffic into tenant namespaces ([#28](https://github.com/ithaca-lab-org/idp-portal/issues/28)) ([896e5ba](https://github.com/ithaca-lab-org/idp-portal/commit/896e5badcef73b5458f3cdc68ec1466d7f105c57))
* **operator:** container image built and pushed by the image workflow ([#7](https://github.com/ithaca-lab-org/idp-portal/issues/7)) ([8f4cbaa](https://github.com/ithaca-lab-org/idp-portal/commit/8f4cbaa00cdc3ba9584d8152d65f0dec0bf1e567))
* **operator:** keep tenant hostnames in the wildcard-apps certificate ([#32](https://github.com/ithaca-lab-org/idp-portal/issues/32)) ([01b5f72](https://github.com/ithaca-lab-org/idp-portal/commit/01b5f72ed7bded62fb34f6a9796c1050f43c6f2e))
* **operator:** ManagedSecret CRD and controller (pinned VaultStaticSecret) ([#22](https://github.com/ithaca-lab-org/idp-portal/issues/22)) ([62b6839](https://github.com/ithaca-lab-org/idp-portal/commit/62b68394bd7c9b2a4e44730558e6bd186019ee94))
* **operator:** per-namespace Vault access and KV purge, behind VAULT_ENABLED ([#19](https://github.com/ithaca-lab-org/idp-portal/issues/19)) ([e9afff1](https://github.com/ithaca-lab-org/idp-portal/commit/e9afff1deb9c038553cdc4504b8d06d48d33c4db))
* **operator:** route to the workload Service, optional Spinnaker deployer binding, reserve spinnaker, pin tenant Services ([#45](https://github.com/ithaca-lab-org/idp-portal/issues/45)) ([ba7a747](https://github.com/ithaca-lab-org/idp-portal/commit/ba7a747f609361c3d45250af32739c4a2d973f1c))
* **operator:** TeamNamespace CRD and reconciler ([#5](https://github.com/ithaca-lab-org/idp-portal/issues/5)) ([aefba1e](https://github.com/ithaca-lab-org/idp-portal/commit/aefba1e7a60710a410aa1fefe1826ff76990db8a))
* **operator:** WorkloadIdentity controller ([#24](https://github.com/ithaca-lab-org/idp-portal/issues/24)) ([fddc871](https://github.com/ithaca-lab-org/idp-portal/commit/fddc8717996eb850e2f310d346d4c9a6dd81f589))
* **portal:** Resources page and API for buckets and grants (ADR 0010 step 3) ([#35](https://github.com/ithaca-lab-org/idp-portal/issues/35)) ([5d37323](https://github.com/ithaca-lab-org/idp-portal/commit/5d37323ef83d28e4f6d83426a85145313e5a12db))
* **portal:** Workloads page and API for WorkloadIdentity ([#26](https://github.com/ithaca-lab-org/idp-portal/issues/26)) ([cf7adb3](https://github.com/ithaca-lab-org/idp-portal/commit/cf7adb3db46c62c4500f6308df4b3b938711a8a8))
* **portal:** write-only Secrets page behind SECRETS_ENABLED ([#21](https://github.com/ithaca-lab-org/idp-portal/issues/21)) ([a88e09f](https://github.com/ithaca-lab-org/idp-portal/commit/a88e09fd8754104de3d9366d2fa162f4ea894d62))
* **vault:** clients trust the Vault CA (VAULT_CACERT) and default to https ([#23](https://github.com/ithaca-lab-org/idp-portal/issues/23)) ([3f418e4](https://github.com/ithaca-lab-org/idp-portal/commit/3f418e417aecc714e460a6dbf00a9639d0d7c47c))
* **web:** React UI served by portal-api ([#10](https://github.com/ithaca-lab-org/idp-portal/issues/10)) ([d9c2049](https://github.com/ithaca-lab-org/idp-portal/commit/d9c2049aa2add05d8897a4712af2eaf0b48e03db))


### Bug Fixes

* **broker:** allow core events in idp-system (leader-election event was rejected) ([#37](https://github.com/ithaca-lab-org/idp-portal/issues/37)) ([1711395](https://github.com/ithaca-lab-org/idp-portal/commit/1711395ce103c230862d7f5e25bd3239c3d838b8))
* **broker:** install the signal handler once (panic when the broker is enabled) ([#36](https://github.com/ithaca-lab-org/idp-portal/issues/36)) ([cb0fd84](https://github.com/ithaca-lab-org/idp-portal/commit/cb0fd84ad0ae33daf22787c5ca7cfc55566bcbab))
* **deps:** Go 1.26.11 for GO-2026-6859 (net/http) ([#46](https://github.com/ithaca-lab-org/idp-portal/issues/46)) ([ac3af21](https://github.com/ithaca-lab-org/idp-portal/commit/ac3af211c6f9f90a41ca3beda48f7b08c723f7a7))
* go-jose 4.1.6 and MongoDB 7.0 for CI (8.x refuses to start on Linux 6.19+ runners) ([#20](https://github.com/ithaca-lab-org/idp-portal/issues/20)) ([e4351ec](https://github.com/ithaca-lab-org/idp-portal/commit/e4351ec64b79ed3543d82fd71afb83d7683f18e0))
* **operator:** accept vproof-* TeamNamespaces; reserve them in the portal API only ([#27](https://github.com/ithaca-lab-org/idp-portal/issues/27)) ([94c6e27](https://github.com/ithaca-lab-org/idp-portal/commit/94c6e27b5b8ec6b91dad55386eaebe9fd9dc6d2a))
* **operator:** pin all tenant NetworkPolicies and namespace labels; privacy hint for namespace names ([#39](https://github.com/ithaca-lab-org/idp-portal/issues/39)) ([4375e8a](https://github.com/ithaca-lab-org/idp-portal/commit/4375e8a79430d40ed6594d69323bb716d56dc48b))
* **web:** keep pages alive, refresh namespaces, share the selected namespace, ignore stale responses ([#38](https://github.com/ithaca-lab-org/idp-portal/issues/38)) ([1969224](https://github.com/ithaca-lab-org/idp-portal/commit/196922452c82236211c005c2a9a63ac6d2a54540))
