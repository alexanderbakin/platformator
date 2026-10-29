# Changelog

Notable changes to Platformator. The operator image is tagged `vX.Y.Z` on a
release and `<latest tag>-build.<N>` on every other push to `main`.

## Unreleased

### Added
- GitHub push webhooks (`--github-webhook-bind-address`, `GITHUB_WEBHOOK_SECRET`, `make deploy-webhook`): a push starts the matching Apps' builds at once, and polling stays as the fallback.
- A `Terraform` workflow: `terraform fmt -check`, Checkov, TFLint and `terraform validate`, from pinned versions.
- Custom domains: `spec.domains` serves an App on names of its own, each with its own certificate, reported in a `DomainsReady` condition and a `DOMAINS` column. Names under the platform domain are refused, and the older App keeps a contested name.
- Sealed Secrets on the node, with a private key made once by `sealed-secrets/generate-key.sh` and installed by Ansible (`sealed_secrets_key_file`), so files sealed for one cluster open on the next.
- Private repositories over SSH: `spec.source.git.deployKeySecret` with an SSH address, host keys pinned from the Secret's `known_hosts`, and a clone in an init container so the key never reaches the build container.

### Changed
- `ACME_EMAIL` is optional: the Let's Encrypt `ClusterIssuer` leaves out its contact address when it is empty.
- The EC2 instance accepts only IMDSv2, has an encrypted root volume and is EBS-optimized, the security group rules have descriptions, and the VPC's default security group has no rules. Changing the root volume replaces the instance, so apply it on a fresh cluster.

## 0.9.0 - 2026-10-27

### Added
- Rebuilds of an App's pods when a Secret or ConfigMap it reads changes, with a revision annotation on the pod template.
- Pod hardening: no privilege escalation and the default seccomp profile always, and non-root, a read-only filesystem and dropped capabilities under `spec.security`.
- A NetworkPolicy per App that admits only the ingress controller, with an opt-in for other traffic.
- A `HostTaken` Ready reason when two Apps claim the same host.
- Tenant namespaces with a ResourceQuota, a LimitRange and a Role (`examples/tenant`), ValidatingAdmissionPolicies and Pod Security labels.
- Build timeout, a limit on concurrent builds and a registry layer cache for Git builds.
- `observedGeneration` on every status condition.
- Metrics: `platformator_apps`, `platformator_app_builds`, `platformator_builds_total` and `platformator_build_duration_seconds`.
- Prometheus, Grafana, Loki and Alloy on the node, installed by Ansible, and a Platformator dashboard with alert, log and autoscaling panels.
- Six alert rules: operator down, reconcile errors, App not Ready, quota refusal, failed build, autoscaler at its maximum.
- `--registry-insecure` (`REGISTRY_INSECURE`) to push builds over plain HTTP to a registry inside the cluster.
- An end-to-end test that builds an image from Git (`make test-e2e-build`, its own non-gating workflow).
- `govulncheck` and a Trivy scan of the published image, an SBOM and provenance on the image, keyless cosign signatures, and Renovate.

### Changed
- The operator's ClusterRole is reduced to the calls the controller makes: no `delete` or `patch`, and only status writes on Apps.
- Build Pods carry `platformator.alexanderbakin.com/build-of` instead of the App's `app` label.

### Fixed
- Finished build Pods matched the App's selector, so the autoscaler counted them as pods with no metrics and stayed at one replica under load.
- The at-maximum-replicas alert fired for Apps whose minimum equals their maximum.

## 0.8.0

- Builds from Git: a rootless BuildKit Job per commit, images pushed to a registry, branch polling.
