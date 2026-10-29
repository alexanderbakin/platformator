# platformator

A tiny PaaS built as a 30-day challenge. A Kubernetes Operator (CRDs + Controller) for deploying applications that autoscale on AWS.

## Status

Day 23 of 30.

Single-node k3s cluster on AWS, reachable over a real domain, serving trusted Let's Encrypt certificates via cert-manager. Apps are deployed through an `App` CRD and a controller (Kubebuilder, Go) that creates and keeps in sync a Deployment, Service, Ingress and HorizontalPodAutoscaler for each App.

What the operator does today:

- **Per-App spec:** image, port, optional `healthCheck` (`path`, `livenessPath`), optional `env` and `envFrom` (Secrets and ConfigMaps), required CPU/memory requests and limits (checked by a CEL rule on the CRD), and min/max replicas plus a target CPU utilization that become an HPA. The CRD rejects bad Apps up front, see [Validation](#validation).
- **Health probes:** every App gets a readiness probe, a TCP connect on its port, so a pod whose process isn't listening is never counted as available. With `healthCheck.path` (for example `/healthz`, must start with `/`) readiness becomes an HTTP request on that path. It may check dependencies such as a database: a failing readiness probe only takes the pod out of the Service. Liveness is a separate, optional `healthCheck.livenessPath` (for example `/livez`) and is never derived from `path`, because a failing liveness probe restarts the container, and a liveness check that depends on a database would restart every pod at once during a database outage. There is no default liveness probe, because a wrong guess would restart healthy apps.
- **Public URL and TLS status:** `status.url` holds the address the App is served on, and the `CertificateReady` condition follows the cert-manager Certificate for it (`True` once issued, `False` with cert-manager's own reason while pending, `Unknown` if cert-manager isn't installed).
- **Status conditions:** `Reconciled` means every API call in the last pass succeeded. `Ready` means the Deployment's rollout is complete and Available and, if cert-manager is installed, the TLS certificate is issued. `Ready` stays False while a rollout is in progress or stuck, even if old pods are still serving, and while the certificate is pending.
- **Events:** each App gets Kubernetes Events when its `Ready` or `Reconciled` condition changes and when its certificate is issued, so `kubectl describe app <name>` shows the history (rollout in progress, certificate pending, ready, failures). Events are sent on changes only, not on every reconcile pass.
- **Config reload and security:** pods restart by themselves when a Secret or ConfigMap they read changes, and every pod gets no privilege escalation and the default seccomp profile, with non-root, read-only filesystem and dropped capabilities available under `spec.security`. See [Configuration and Secrets](#configuration-and-secrets) and [Security defaults](#security-defaults).
- **Isolation and build safety:** every App gets a NetworkPolicy that lets only the ingress controller in, an App whose hostname belongs to an older App in another namespace waits with `HostTaken`, a quota that refuses pods is reported on `Ready` as `ReplicaFailure`, and builds have a timeout, a concurrency limit and resource limits. See [Network isolation](#network-isolation), [Teams and quotas](#teams-and-quotas) and the [Threat model](#threat-model).
- **Builds from Git:** instead of an `image`, an App can name a public repository. The operator watches the branch, builds the Dockerfile in the cluster with BuildKit, pushes to GHCR and rolls the Deployment to the new image. See [Building from Git](#building-from-git).
- **Custom domains:** `spec.domains` serves an App on names of your own, each with its own Let's Encrypt certificate, next to its address under the platform domain. A separate `DomainsReady` condition reports them, so a name whose DNS is not ready yet does not make a working App look broken. See [Custom domains](#custom-domains).
- **Metrics and a dashboard:** besides the controller-runtime metrics, the operator exports `platformator_apps` (Apps by `Ready` status and reason), `platformator_app_builds` (latest build state), `platformator_builds_total` and `platformator_build_duration_seconds`. Prometheus, Grafana, Loki and Alloy come with the node. A dashboard in the repo shows the Apps, the builds, the operator, the autoscaling and the logs of one App, and alert rules cover the operator, the builds and the Apps. See [Observability](#observability).
- **In-cluster deployment:** runs from an image on GHCR. The base domain comes from a gitignored `operator.env` through a Kustomize-generated ConfigMap, so it never lands in git.
- **Tests:** envtest-based tests for the controller, a kind-based e2e suite that runs the real operator as its real ServiceAccount, and GitHub Actions running lint, unit tests and e2e on every push.

## Architecture

Three layers, each built by a different tool:

```
Terraform  ->  AWS: EC2 instance, Route53 zone for the subdomain, state bucket
Ansible    ->  the node: k3s (with Traefik), cert-manager, a Let's Encrypt ClusterIssuer, Prometheus, Grafana and Loki
operator   ->  the platform: an App CRD and a controller that runs inside k3s
```

**The operator** is a Kubebuilder (Go) controller. A user applies one `App` object; the controller turns it into everything needed to serve it:

```
                         +--------------------+
  kubectl apply App ---> |  App (CRD)         |
                         +---------+----------+
                                   | reconcile
        +------------+-------------+--------------+----------------+
        v            v             v              v                v
   Deployment     Service       Ingress          HPA        build Job (BuildKit)
   (probes,       (port)     (host, TLS via     (min/max,    only with spec.source
    env, image)               cert-manager)      CPU target)
```

Each pass of the controller, in order:

1. **Build** (only with `spec.source`): read the branch head from the Git server, start a BuildKit Job for a commit that has none, and pick the image of the newest successful build.
2. **Deployment, Service, Ingress, HPA:** create or update each one so it matches the spec, owned by the App so deleting the App removes them.
3. **Status:** read the Deployment, the build and the cert-manager Certificate, and write the `Reconciled`, `Ready`, `CertificateReady` and `Built` conditions, plus `status.url` and `status.build`. Events are sent only when a condition changes.

Things worth knowing about how it behaves:

- **The controller never edits `spec`.** It only writes `status`. What you applied is what you get, which is why a rollback is a Git revert and not an operator feature.
- **Watched vs polled.** Deployments, Services, Ingresses, HPAs and build Jobs are watched through ownership. Certificates and Git branches are polled (10 seconds while pending, then minutes), because the cert-manager CRD may not be installed and Git has no event stream without a webhook.
- **cert-manager is optional.** The Certificate is read as unstructured, so without cert-manager `CertificateReady` is just `Unknown` and does not block `Ready`.
- **Failures stay local.** A conflict is retried quietly, an unreachable Git host keeps the last good image running, and a failed build never replaces it.
- **Config comes from the environment.** Base domain, registry and the BuildKit image are flags with env fallbacks, fed by a gitignored `operator.env` through a Kustomize ConfigMap.


## Design Decisions

Each entry says what was chosen, why, and what it costs.

**An operator with one CRD, not a Helm chart or a script.** An `App` is a single object that the cluster keeps true, instead of a template rendered once. The controller fixes drift, reports status and reacts to changes. Cost: more code to own than a chart, and a CRD to keep compatible.

**The operator only writes `status`, never `spec`.** It never changes what you applied. A rollback is a Git revert, which keeps Git the source of truth. A `lastReadyImage` field was built and removed for this reason: a second place that remembers an image would disagree with Git. Cost: no one-command rollback inside the platform.

**`Ready` is strict.** It is False while a rollout is in progress or stuck and while the certificate is pending, even if old pods still serve. Available=True alone hides crashlooping new pods during a rolling update. Cost: `Ready` flaps during every deploy. A separate `Progressing` condition was considered and dropped as extra surface for little gain.

**Validation lives in the CRD (CEL), not in a webhook.** Rules like "resources are required" and "set exactly one of image and source" run in the API server, so they work in envtest and need no webhook server or certificate. Cost: CEL is less expressive than Go, and cannot check things outside the object.

**Readiness by default, liveness only when asked.** Every App gets a TCP readiness probe so Ready means something. Liveness is opt-in and never reuses the readiness path: a readiness path may check a database, and a failing liveness probe restarts the container, so sharing them would restart every pod during a database outage. Cost: apps that need liveness must say so.

**Events on change only.** Events are sent when a condition's status or reason changes, and only after the status update succeeded. `kubectl describe app` shows a history, not a repeat of every pass.

**Poll what cannot be watched.** Certificates are read as unstructured and polled, so the operator needs neither cert-manager's Go module nor its CRD. Git branches are polled once a minute over plain HTTPS. Cost: a certificate or commit change can take up to the poll interval to show.

**Builds run as BuildKit Jobs in the cluster.** The operator does not build anything itself, and it needs no Docker daemon: a Job runs rootless BuildKit against the repository at an exact commit and pushes to GHCR. The Deployment moves only when a build succeeded, so a broken commit never takes the App down. A failed commit is not retried. Cost: the node must allow the rootless profile (seccomp and AppArmor unconfined), and only public repositories work for now.

**Registry credentials are a personal access token in a Secret.** The build Job and the Deployment use a classic PAT from a `registry-credentials` Secret. GitHub can't scope a classic token below `repo` when it has `write:packages`, and fine-grained tokens don't reach GHCR, so the token can read and write private repositories. That is acceptable for a one-person demo cluster. A real setup would use a machine account that owns no repositories, or build in CI with the per-run `GITHUB_TOKEN`.

**Config changes restart pods through an annotation, not a data hash.** The operator fingerprints the `resourceVersion` of the Secrets and ConfigMaps an App uses and puts it on the pod template. It never reads the data, so credentials stay out of its memory. Cost: Kubernetes cannot give metadata-only access, so the operator can `get`, `list` and `watch` every Secret and ConfigMap, and a metadata-only change (a label) restarts pods too. Alternatives were a hash of the data (needs the same permissions and reads the values), a tool like Reloader (another component), and immutable Secrets (a new name per change).

**Hardening that can break an image is opt-in.** No privilege escalation and the default seccomp profile are always on. Non-root, a read-only filesystem and dropped capabilities are `spec.security` options, because a default that stops nginx or postgres from starting is a worse platform than one that asks. Cost: Apps are not hardened until someone asks.

**Network isolation allows only ingress, not egress.** Every App gets a policy that lets only the ingress controller in. Restricting egress as well would stop Apps from reaching the internet and databases until each one listed its destinations, which is too much to ask of a small platform. Cost: a compromised App can still reach out, and `allowSameNamespace` is the only way for Apps to call each other.

**The oldest App keeps a hostname.** An App is served on `<name>.<domain>` with no namespace in it, so same-named Apps would collide. Putting the namespace in the hostname would fix that but change every URL, so the older App keeps the address and the newer one waits with `HostTaken`. Cost: whoever creates a name first holds it.

**Builds are limited by timeout, concurrency and resources, and fail closed on missing credentials.** One node cannot run many BuildKit pods at once, so a push to several Apps queues instead of piling up. A missing registry Secret is reported before a Job is created, since a pod waiting for it shows nothing on the App. Cost: a build can wait behind others, and a long build is cut off at the timeout.

**Polling Git instead of webhooks.** A webhook needs a public endpoint on the operator, a secret and per-provider setup. Polling works with any HTTPS Git host and nothing exposed. Cost: up to a minute of delay and one request per App per minute.

**One node, on purpose.** The target is a single k3s node on AWS: cheap, easy to rebuild from Terraform and Ansible, and enough to show the platform. Cost: no high availability. The operator itself would not need to change to run on more nodes.

**App metrics are computed when Prometheus scrapes, not kept as counters.** `platformator_apps` lists the Apps from the informer cache on every scrape. A gauge that the reconciler increments and decrements would drift on deletes and restarts, and would need cleanup of label sets that no longer exist. Cost: a list per scrape (15-30 seconds apart, from memory, so cheap). Build results are the opposite: they are events, so they are counters, incremented once when the `Built` condition changes, after the status is stored, like the Events.

**Prometheus and Grafana come from the Helm controller k3s already has, and Grafana is not exposed.** A `HelmChart` manifest written by Ansible installs kube-prometheus-stack with Alertmanager and the control-plane scrapes off (k3s binds those to localhost), three days of retention and no volumes. Grafana is reached with `kubectl port-forward`, so no public endpoint, certificate or password policy is needed for a dashboard used by one person. Cost: the data is gone when the node is. That matches the rest of the setup, and it is why alert rules (Day 20) go in the repo too. The operator scrape uses `insecureSkipVerify`, because the operator's metrics certificate is self-signed and the traffic stays inside the cluster.

**Alerts are rules in the repo, without Alertmanager.** A `PrometheusRule` holds six alerts that each point at a thing to do. Nothing is sent: on a demo cluster that is up for a few hours, a notification channel would be more setup than signal, and the state is one click away in Prometheus and on the dashboard. Cost: nobody is told when something is wrong. Adding Alertmanager later is a chart value and a receiver, the rules would not change.

**Logs go to Loki through Alloy, with no host access.** Alloy reads logs from the Kubernetes API, so it needs no hostPath mounts and no privileged DaemonSet, and a single replica is enough for one node. Loki runs as one binary on the filesystem without a volume. Cost: logs die with the Pod, lines written while Loki is down are dropped (Alloy retries briefly, then gives up, and tails only new lines once Loki is back), and tailing through the API does not scale to a large cluster. Both are fine here and would change first if the cluster grew.

**The Git build test is separate from the gating e2e.** It exercises the whole path (resolve the head, run rootless BuildKit, push) against a registry in the cluster, which the unit tests cannot. It depends on user namespaces on the runner and on Docker Hub, so a failure there says little about the operator and it must not block publishing an image. Cost: a regression in the build path can reach main before this test notices.

**The operator gets only the permissions it calls.** The role was the Kubebuilder default, full verbs on everything. I went through the code instead: children are made and changed with `CreateOrUpdate` and removed by the garbage collector through their owner reference, so there is no `delete` and no `patch`; Apps are read and only their status is written; Jobs are created and watched, never changed. What remains is `get`, `list`, `watch`, `create` and `update` on the children, `update` on `apps/status`, and read access to Secrets and ConfigMaps in every namespace, because an App can point at one anywhere. RBAC cannot limit that read to metadata, so the operator asks for metadata only, but the permission is still broad. A bug that needs a verb the role no longer has fails loudly, and the e2e suite runs with the real role.

**The image is scanned after it is pushed, and a signature means it passed.** Scanning before the push would need the image built and loaded twice. Instead the multi-architecture image is pushed, scanned, and signed by digest only if the scan passes, so the signature carries the verdict and the registry's contents don't. Cost: a vulnerable image is briefly pullable, unsigned. Signing is keyless (the run's identity, not a long-lived key), so there is no key to leak, and verifying needs the workflow's name and the GitHub issuer. The scanner and signer run from pinned versions as plain commands, not as third-party actions, which keeps what runs in the release workflow to what is written in it.

**CI gates the image.** The operator image is published only after lint, unit tests and e2e on kind pass, so what lands on GHCR has been tested.

**A private repository is cloned by git, not by BuildKit.** BuildKit can fetch an SSH Git context with a forwarded key, but it decides by itself which host keys to accept. An init container running git with `StrictHostKeyChecking=yes` and a `known_hosts` from the same Secret keeps that decision in the operator's hands, and keeps the deploy key out of the container that runs untrusted Dockerfile steps. The branch lookup uses the same Secret and the same host key check, through Go's SSH client, so the operator does not need a git or ssh binary. Cost: the build image must contain git and ssh (the BuildKit image does), and a build of a private repository does one extra step before BuildKit starts.

**A webhook only hurries the poll along.** The push handler does not carry a commit into the build: it asks the controller to look at the matching Apps now, and the controller finds the branch head the same way it does every minute. A lost, duplicated or forged-and-rejected delivery therefore cannot start a wrong build or skip one, and the worst case without webhooks is the polling delay. Cost: one more public path on the node, guarded by an HMAC, and a second place that has to know the secret.

**Every custom domain gets its own certificate and a condition of its own.** One certificate for all the names would be simpler, but HTTP-01 issues it only when every name validates, so one name with a wrong DNS record would hold up the others and the App's own address. A certificate and a Secret per name keeps a bad name to itself. The Secret is named from a hash of the domain, not its position in the list, so reordering the list does not issue everything again. `DomainsReady` is separate from `Ready` because a pending name is the domain owner's DNS to fix, not a failure of the App, and tying them together would raise "App not Ready" for an App that serves fine. Names are first come, first served, the older App keeps a contested one, because the operator cannot know who owns a domain. Cost: more certificates, and Let's Encrypt's limits count certificates per registered domain, so many Apps with several names under one domain could reach them, especially when every certificate is asked for again after a rebuild. Nothing proves who owns a name beyond its DNS and the challenge.

**Secrets are committed sealed, with a key that lives outside the cluster.** Sealed Secrets encrypts a Secret to a public certificate, so the encrypted file can sit in Git next to the App manifests, and a controller in the cluster turns it back into an ordinary Secret. The operator needs no change, because Apps already read ordinary Secrets through `envFrom`. The catch with a node that is destroyed often is the private key: the controller makes its own on first start, and files sealed for the last cluster would not open on the next one. So I generate the key pair once with a script, keep the private half in a file outside the repo, and Ansible installs it before the controller starts, with key renewal turned off so there is exactly one key. Cost: that file is the one thing to back up, losing it means sealing every secret again, and whoever holds it can open every sealed file.

**External Secrets and the Secrets Store CSI driver were considered and left out.** The External Secrets Operator would keep the values in AWS, which survives the cluster with no key to back up, but a pod has to authenticate to AWS to read them. The node accepts only IMDSv2 and leaves the hop limit at its default of 1, so pods cannot reach the instance's credentials, which is the point of that setting. Allowing it would give every pod on the node a route to the node's role, and a static IAM key would need a Secret created by hand, the problem being solved. The CSI driver mounts values as files and mirrors them into a Secret only while a pod mounts them, which does not fit `envFrom`, build Jobs or the operator's own Secrets without changing what the operator creates. Cost: no central store to rotate from. On a cluster with a per-pod AWS identity (EKS, for example) External Secrets would be the better answer.

## Getting started

### Set up your environment

Everything below is driven by a handful of values specific to your setup. Collect them once, up front, rather than re-typing them at each step:

```bash
regions=()
while IFS= read -r region; do
  regions+=("$region")
done < <(aws ec2 describe-regions --query "Regions[].RegionName" --output text | tr '\t' '\n' | sort)

PS3="AWS region: "
select AWS_DEFAULT_REGION in "${regions[@]}"; do
  [ -n "$AWS_DEFAULT_REGION" ] && break
done
export AWS_DEFAULT_REGION

read -p "Root domain's Route53 hosted zone name, e.g. example.com. (must end in a dot): " HOSTED_ZONE_NAME
export HOSTED_ZONE_NAME

read -p "Subdomain to serve the platform on, e.g. paas.example.com: " DOMAIN
export DOMAIN

read -p "Email for the Let's Encrypt account (optional, press Enter to skip): " ACME_EMAIL
export ACME_EMAIL
```

These stay exported for the rest of this walkthrough. The Ansible dynamic inventory and the k3s and cert-manager config read `AWS_DEFAULT_REGION`, `DOMAIN` and `ACME_EMAIL` from the environment rather than a committed file, since they're specific to your setup. `ACME_EMAIL` is optional: Let's Encrypt issues certificates either way, and without it the account is registered with no contact address. `HOSTED_ZONE_NAME` is used only by the Route53 commands below, because Terraform reads the zone from `terraform.tfvars`.

### Bootstrap the state bucket

The S3 backend needs a bucket to exist before `terraform init` can use it:

```bash
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export BUCKET_NAME="tf-state-${AWS_ACCOUNT_ID}-${AWS_DEFAULT_REGION}-an"

aws s3api create-bucket \
  --bucket "${BUCKET_NAME}" \
  --bucket-namespace account-regional \
  --region "${AWS_DEFAULT_REGION}" \
  --create-bucket-configuration LocationConstraint="${AWS_DEFAULT_REGION}"

aws s3api put-bucket-versioning \
  --bucket "${BUCKET_NAME}" \
  --versioning-configuration Status=Enabled
```

Versioning is the safety net here - if `terraform.tfstate` ever gets corrupted or clobbered, you can roll back to a previous version instead of losing state entirely.

### Initialize Terraform against it

The bucket name isn't hardcoded in `versions.tf` on purpose - bucket names are effectively global and tie back to your AWS account, so it's passed in at init time instead of committed:

```bash
cd terraform
terraform init \
  -backend-config="bucket=${BUCKET_NAME}" \
  -backend-config="region=${AWS_DEFAULT_REGION}"
```

### Create a Route53 hosted zone for your subdomain

Terraform looks up a hosted zone by name at apply time rather than creating one itself, so a zone has to exist first. If you don't already have one for `$HOSTED_ZONE_NAME`:

```bash
aws route53 create-hosted-zone \
  --name "$HOSTED_ZONE_NAME" \
  --caller-reference "$(date +%s)"
```

The response includes a `DelegationSet.NameServers` list (four hostnames) and the zone's `Id`. At your domain's registrar, add those four as `NS` records - delegation typically settles within an hour.

If you already manage this domain in Route53 - say, from another project - skip creation and just look up the existing zone instead:

```bash
aws route53 list-hosted-zones-by-name --dns-name "$HOSTED_ZONE_NAME"
```

Either way, add it to `terraform.tfvars` along with the subdomain:

```hcl
hosted_zone_name = "<the hosted zone name you entered above>"   # must end in a dot
domain           = "<the domain you entered above>"
```

### Apply

```bash
terraform apply
```

This provisions the VPC, the EC2 instance, its Elastic IP, and the `$DOMAIN` + wildcard DNS records pointing at that IP.

### Configure the node with Ansible

Install dependencies:

```bash
cd ../ansible
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
ansible-galaxy collection install -r requirements.yml
```

Run the playbook:

```bash
ansible-playbook playbook.yml
```

This installs and starts k3s, bootstraps cert-manager and a `letsencrypt` `ClusterIssuer` via k3s's manifest auto-deploy (`/var/lib/rancher/k3s/server/manifests` - no Helm or manual `kubectl apply` needed), and fetches the kubeconfig back to `ansible/kubeconfig`, already patched to point at the node's public IP instead of `127.0.0.1`.

The `ClusterIssuer` uses the HTTP-01 challenge type against the k3s-bundled Traefik, so no DNS-provider API credentials are needed - port 80 is already open (see `terraform/main.tf`).

By default k3s, cert-manager and the monitoring stack (kube-prometheus-stack) install whatever's currently latest. To pin them for a reproducible provision:

```bash
ansible-playbook playbook.yml -e k3s_version=v1.32.1+k3s1 -e cert_manager_version=v1.21.2 -e kube_prometheus_stack_version=<chart version>
```

To keep Secrets sealed in Git openable on every new node, also pass the Sealed Secrets key, see [Sealed Secrets](#sealed-secrets).

### Use kubectl

```bash
export KUBECONFIG=./ansible/kubeconfig
kubectl get nodes
```

### Deploy an app via the App CRD

Day 4 replaces manually applying a Deployment/Service/Ingress with an `App` CRD and controller that create them for you, wired up the same way as before: Traefik ingress, TLS via the `letsencrypt` `ClusterIssuer`.

Install the CRD into the cluster:

```bash
cd operator
make install
```

Either run the controller locally against the cluster (it reads the base domain from `--domain` or the `DOMAIN` env var you exported earlier):

```bash
make run
```

Or run it in the cluster. The base domain goes in a gitignored file that Kustomize turns into a ConfigMap:

```bash
cp config/default/operator.env.example config/default/operator.env   # then set DOMAIN=<your domain>

make deploy-image IMG=ghcr.io/<you>/platformator-operator:v0.6.0-build.42
```

CI builds and publishes the image, so there is no local build step. Every push to `main` that touches `operator/` runs the `Publish image` workflow, which pushes a `linux/amd64` + `linux/arm64` image tagged `<latest version tag>-build.<run number>`, for example `v0.6.0-build.42`. Pushing a git tag such as `v0.6.0` publishes it under exactly that name, which is how a release is cut. The workflow refuses to overwrite a tag that already exists, and fails if either platform is missing from the pushed image. The run summary shows the exact `make deploy-image` command. `deploy-image` builds the manifests from a temporary copy of `config/`, so it never changes a tracked file and a deploy never needs a commit.

To build by hand instead (for example from an uncommitted change):

```bash
make docker-buildx IMG=ghcr.io/<you>/platformator-operator:<tag> PLATFORMS=linux/amd64
make deploy-image IMG=ghcr.io/<you>/platformator-operator:<tag>
```

A few things that are easy to get wrong here:

- `PLATFORMS` must match your nodes. On an arm64 Mac, a plain `docker-build` produces an image that fails with "exec format error" on amd64 nodes.
- Use a new tag for every build and never overwrite one. Nodes cache images by tag, so a reused tag can leave the old code running.
- The nodes must be able to pull the image: make the GHCR package public or configure an `imagePullSecret`.
- Use `make deploy-image`, not `make deploy`. The scaffolded `make deploy` rewrites the image in `config/manager/kustomization.yaml`, and committing that change only triggers another CI build that is one commit behind.

In another terminal, apply a sample App:

```bash
kubectl apply -f config/samples/platformator_v1alpha1_app.yaml
kubectl get certificate hello-tls -w   # wait for READY=True
curl "https://hello.$DOMAIN/"
```

No `-k`, no warnings - a real Let's Encrypt cert, same as before, now provisioned by the operator instead of by hand.

### Check an App's status

```bash
kubectl get app -w
```

The default columns are `Ready`, the reason for `Ready`, and the age. `kubectl get app -o wide` adds the image, the public URL, `Reconciled`, and the TLS status (the `CertificateReady` condition). For example, an App whose image can't be pulled stays `Reconciled=True` (the API calls worked) but `Ready=False`:

| Ready | Reason | Meaning |
|---|---|---|
| `True` | `DeploymentAvailable` | Rollout complete and Available |
| `False` | `DeploymentUnavailable` | The Deployment has no minimum availability, for example the pod can't start or fails its readiness probe |
| `False` | `RolloutInProgress` | A rollout is still going or stuck, even if old pods still serve |
| `False` | `CertificateNotReady` | The pods are fine but the TLS certificate isn't issued yet, see the message and the `CertificateReady` condition |
| `False` | `ProgressDeadlineExceeded` | The rollout made no progress within the Deployment's deadline |
| `False` | `ReconcileError` | The last reconcile pass failed, see the `Reconciled` condition |

The TLS column (`-o wide`) and `kubectl describe app` show the certificate state:

| CertificateReady | Reason | Meaning |
|---|---|---|
| `True` | `CertificateIssued` | cert-manager issued the certificate |
| `False` | `CertificatePending` | cert-manager has not created the Certificate or reported its state yet |
| `False` | cert-manager's reason (for example `DoesNotExist`, `Issuing`) | Issuance is in progress or failing, see the message |
| `Unknown` | `CertManagerNotInstalled` | The Certificate CRD isn't installed, so there is nothing to check |
| `Unknown` | `CertificateCheckFailed` | The operator could not read the Certificate, see the message |

An App whose certificate isn't issued yet is checked again every 10 seconds, and one whose certificate is fine every 10 minutes, so a renewal that fails turns `Ready` False again.

`kubectl describe app <name>` also lists the Events, for example:

```
Events:
  Type    Reason                 Age   From            Message
  Normal  ReconcileSucceeded     40s   app-controller  Deployment, Service, Ingress and HorizontalPodAutoscaler are up to date
  Normal  DeploymentUnavailable  40s   app-controller  Deployment does not have minimum availability.
  Normal  CertificateNotReady    36s   app-controller  TLS certificate is not issued yet: ...
  Normal  CertificateIssued      14s   app-controller  Certificate is up to date and has not expired
  Normal  DeploymentAvailable    14s   app-controller  Deployment is fully rolled out and available
```

### Configuration and Secrets

Put configuration in `env`, and credentials in a Secret that `envFrom` (or an `env` entry with `valueFrom`) points to, so they stay out of the App manifest:

```yaml
spec:
  envFrom:
    - secretRef:
        name: db-credentials
    - configMapRef:
        name: settings
```

Every key of the Secret or ConfigMap becomes an environment variable. Each entry sets exactly one of `secretRef` and `configMapRef`. The objects must exist in the App's namespace: until they do, the pod cannot start (`CreateContainerConfigError`) and the App stays not Ready, then it starts by itself once they exist. The operator never reads the values: only the pod does, and the operator looks at the object's metadata (see below).

Pods read these values when they start, so a change has to restart them. The operator does that for you: it watches the Secrets and ConfigMaps an App uses (through `envFrom` and `env.valueFrom`) and keeps a short fingerprint of their versions in the annotation `platformator.alexanderbakin.com/config-revision` on the pod template. When one changes, the annotation changes and the Deployment rolls out new pods with the new values. A Secret that did not exist yet changes the fingerprint when it appears.

The operator reads only the metadata of these objects, never their data, so a Secret's contents never enter the operator's memory. Kubernetes cannot grant metadata-only access, though, so the operator's role has `get`, `list` and `watch` on Secrets and ConfigMaps in every namespace. An App that uses neither gets no annotation and is never restarted by this. `kubectl rollout restart` still does not stick, because the operator rewrites the pod template on every pass.

### Sealed Secrets

A Secret that belongs in Git is sealed first: it is encrypted to a public certificate, and a controller on the node (installed by the Ansible playbook) turns the committed file back into an ordinary Secret. Apps read it through `envFrom` like any other Secret, and the config reload restarts their pods when it changes. You need [`kubeseal`](https://github.com/bitnami/sealed-secrets#kubeseal) (`brew install kubeseal`).

Make the key pair once:

```bash
sealed-secrets/generate-key.sh
```

It writes two files to `~/.config/platformator/sealed-secrets/`: `key.yaml`, the private key, and `pub-cert.pem`, the public certificate. Back up `key.yaml` outside this repo and this machine (a password manager works) and never commit it. The certificate can only seal, so it is safe to commit if you want it in the repo. The script refuses to overwrite an existing key.

Give the key to Ansible, so every new node starts with it:

```bash
cd ansible
ansible-playbook playbook.yml -e sealed_secrets_key_file=$HOME/.config/platformator/sealed-secrets/key.yaml
```

The playbook puts the key in k3s's manifests directory (mode 0600) and starts the controller with key renewal off, so there is one key and the certificate stays valid for ten years. Without the variable the controller makes its own key, and files sealed for another cluster will not open. If you add the key to a node whose controller has already made one, restart it so it reads the key: `kubectl -n kube-system rollout restart deployment/sealed-secrets-controller`.

Seal a Secret with the certificate, no cluster connection needed:

```bash
kubectl create secret generic db-credentials --namespace default \
  --from-literal=DATABASE_URL='postgres://user:password@host/db' \
  --dry-run=client -o yaml \
  | kubeseal --cert ~/.config/platformator/sealed-secrets/pub-cert.pem --format yaml \
  > db-credentials.sealed.yaml

kubectl apply -f db-credentials.sealed.yaml
kubectl get secret db-credentials
```

The sealed file is safe to commit. By default it is bound to the Secret's name and namespace, so renaming it or moving it to another namespace makes it fail to open: seal it again for the new place. The namespace has to exist before you apply the file, and if a sealed file does not turn into a Secret, `kubectl describe sealedsecret <name>` shows why. On a new cluster you apply the same files again, with no Secrets to create by hand. The Secrets the operator itself needs (`registry-credentials`, `github-webhook`) can be sealed the same way, for their own namespace.

If the key is lost, nothing can open the sealed files: make a new key and seal the secrets again.

### Security defaults

Every App's pods run with no privilege escalation and the container runtime's default seccomp profile. Both are safe for ordinary images. Three more settings can break an image, so they are opt-in under `spec.security`:

```yaml
spec:
  security:
    runAsNonRoot: true            # refuse to start if the image would run as root
    readOnlyRootFilesystem: true  # the app must not write outside volumes
    dropCapabilities: true        # drop all capabilities except NET_BIND_SERVICE
```

Turn them on for an image you know works with them. `dropCapabilities` stops images that start as root and then switch user (nginx, postgres, redis), which is why it is not the default.

### Building from Git

Instead of a ready-made `image`, an App can name a Git repository (set exactly one of the two):

```yaml
spec:
  source:
    git:
      url: https://github.com/alexanderbakin/platformator   # public, HTTPS
      branch: main                                          # default
      path: examples/hello                                  # directory with the Dockerfile, default: root
      dockerfile: Dockerfile                                # default
  port: 80
```

How it works:

- The operator asks the Git server for the head of the branch once a minute (plain HTTPS, no `git` binary, no webhook to expose).
- For a commit it has not built yet, it starts a rootless [BuildKit](https://github.com/moby/buildkit) Job that builds the Dockerfile and pushes `<REGISTRY>/<namespace>-<name>:<commit>`.
- When the Job succeeds, the Deployment moves to that image. While a build runs or fails, the previous image keeps running, so a broken commit never takes the App down. A failed commit is not retried; push a new one.
- `kubectl get app -o wide` shows the image that runs (its tag is the commit), the `Built` condition has the state, and `status.build.jobName` points at the Job whose logs hold the build output.

Setup, once:

```sh
# 1. Where images go: add to config/default/operator.env (not committed)
REGISTRY=ghcr.io/<your-github-name>

# 2. Credentials to push and pull (a token with write:packages), in each App's namespace
kubectl create secret docker-registry registry-credentials \
  --docker-server=ghcr.io --docker-username=<your-github-name> --docker-password=<token>
```

Build safety:

- A build is refused until the `registry-credentials` Secret exists in the App's namespace: `Built` is False with reason `RegistrySecretMissing` and the `kubectl create secret` command to run, and no Job is created. It starts by itself once the Secret exists.
- A build Job is stopped after `--build-timeout` (default 15 minutes) and shows as `Failed` with `DeadlineExceeded`.
- At most `--max-builds` builds run at once in the whole cluster (default 2). Others wait as `Queued` with `Built` Unknown and reason `BuildQueued`.
- Build pods ask for 250m CPU and 512Mi and may use 1 CPU and 1.5Gi, so a ResourceQuota accepts them and one build cannot starve the node.
- BuildKit keeps a layer cache in `<REGISTRY>/<namespace>-<name>:buildcache`, so a build reuses unchanged layers.

Limits for now: one Dockerfile build per commit, and the BuildKit Job runs rootless with seccomp and AppArmor unconfined, which the node must allow. Old images and the cache stay in the registry; deleting them needs a token scope the operator does not hold.

### Private repositories

A private repository is read over SSH with a deploy key. Use an SSH address and name a Secret in the App's namespace:

```yaml
spec:
  source:
    git:
      url: git@github.com:owner/private-repo.git   # or ssh://git@github.com/owner/private-repo.git
      deployKeySecret: repo-deploy-key
```

```bash
# A key pair that belongs to this one repository: add the public half as a read-only deploy key.
ssh-keygen -t ed25519 -N "" -C platformator -f ./deploy-key

# The Git server's public keys, from GitHub's own list. Check them against
# https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
curl -s https://api.github.com/meta | jq -r '.ssh_keys[] | "github.com " + .' > known_hosts

kubectl create secret generic repo-deploy-key \
  --from-file=ssh-privatekey=./deploy-key --from-file=known_hosts=./known_hosts
```

- The branch is looked up over SSH by the operator, and the commit is cloned by an init container in the build Pod. The key is mounted in that init container only, not in the build container that runs the Dockerfile.
- Host keys are checked against `known_hosts` in the Secret, both for the lookup and for the clone. A server that presents any other key is refused, so there is no trust on first use. Use plain host names in the file (`ssh-keyscan` without `-H`).
- `deployKeySecret` is required for an SSH address and not allowed for an HTTPS one. A missing or incomplete Secret shows as `Built` False with reason `DeployKeyMissing` and the command to create it, and is retried by itself.
- The key must not have a passphrase. The operator reads the Secret from the API server each time it checks the branch, so a rotated key is used on the next check.
- The Dockerfile build itself is unchanged: BuildKit builds the cloned directory, so `path` and `dockerfile` work as for a public repository.

### Builds on push

By default the operator checks every branch once a minute. A GitHub webhook makes a push start its build at once. It is optional, and the polling stays on either way, so a lost delivery only costs up to a minute.

```bash
# 1. A shared secret, in the operator's namespace. Create it before the operator starts (or restart the operator after).
openssl rand -hex 32 > webhook-secret
kubectl create secret generic github-webhook -n operator-system --from-literal=secret="$(cat webhook-secret)"

# 2. Turn the listener on: add to config/default/operator.env, then deploy the operator as usual.
GITHUB_WEBHOOK_ADDR=:8082

# 3. Route https://<DOMAIN>/hooks/github to it (DOMAIN from operator.env).
make deploy-webhook
```

Then, in the GitHub repository under Settings, Webhooks, add one with the payload URL `https://<DOMAIN>/hooks/github`, content type `application/json`, the secret from `webhook-secret`, and "Just the push event". GitHub sends a `ping` first. Its delivery page ("Recent Deliveries") shows the operator's answer, `{"triggered":1}` for a push to a branch that one App builds.

To try it without GitHub, send a signed `ping`:

```bash
body='{"zen":"test"}'
sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$(cat webhook-secret)" | sed 's/^.* //')"
curl -i "https://<DOMAIN>/hooks/github" -H "X-GitHub-Event: ping" -H "X-Hub-Signature-256: $sig" -d "$body"   # 200, pong
```

- A delivery without a valid `X-Hub-Signature-256` (HMAC-SHA256 of the body with the secret) gets 401 and nothing is read from it. The listener does not start without the secret.
- A push matches an App when the repository and the branch are the same as in `spec.source.git`. The https and ssh addresses of one repository count as the same, so the webhook also works for private repositories.
- Tags, deleted branches and other event types are acknowledged and ignored.
- The listener is served on the apex of the platform domain (`https://<DOMAIN>/hooks/github`), so no App can take its address, and only that path is routed.

### Custom domains

An App is always served on `<name>.$DOMAIN`. `spec.domains` adds names of your own next to it:

```yaml
spec:
  domains:
    - shop.example.org
    - www.shop.example.org
```

There is a complete example in `examples/custom-domain/app.yaml`. To use it:

1. Point each name's DNS at the cluster. A CNAME to the App's own address (`<name>.$DOMAIN`) works for any subdomain and follows the node when the cluster is rebuilt, because Terraform updates that address. A name that cannot be a CNAME, such as a bare domain, needs an A record with the node's Elastic IP (`terraform output eip`), which changes with every new cluster.
2. Apply the App. cert-manager proves control of each name with the HTTP-01 challenge on port 80 and issues one certificate per name, which is why wildcards are not allowed and the DNS record has to exist first. If it did not yet, cert-manager retries by itself with growing delays, so a name can take a while to turn ready once DNS is fixed.
3. Look at `kubectl get app <name> -o wide`, which has a `DOMAINS` column, and at `kubectl describe app <name>`, where the `DomainsReady` condition says which name is waiting and why.

What to know:

- Names are lowercase DNS names with at least one dot, at most ten per App, and each is checked by the CRD (no wildcards, no IP addresses, no duplicates).
- A name under the platform domain is refused (`DomainNotAllowed`), because those addresses belong to the Apps and to the webhook.
- A name that another App already lists stays with the older App (`DomainTaken`), as with hostnames. The newer App keeps serving its other names and its own address, and gets the name once the older App is gone or stops listing it.
- `Ready` and `status.url` do not depend on the custom domains. A name whose DNS is wrong shows up in `DomainsReady` and nowhere else, and it does not hold up the certificate of the App's own address or of the other names.
- Removing a name from the list stops serving it. The operator does not delete the Secret that holds its certificate (`<name>-tls-<hash>`), so remove it yourself if you want it gone.

### Network isolation

Every App gets a NetworkPolicy named after it. Only the ingress controller's pods (Traefik in `kube-system` on k3s, set with `--ingress-namespace`) may connect, and only to the App's port. Pods of other Apps and other namespaces cannot reach it, and neither can anything else in the cluster. Egress is not restricted: Apps need the internet and their databases.

An App that another App must call through its Service opts in:

```yaml
spec:
  network:
    allowSameNamespace: true   # every pod in this namespace may connect
```

The policy needs a CNI that enforces NetworkPolicy. k3s does (it ships kube-router for this).

### Teams and quotas

Give each team its own namespace. `examples/tenant` is a ready kustomization for one:

```sh
kubectl apply -k examples/tenant      # copy the directory and change team-a for each team
```

It creates the namespace, a ResourceQuota (total CPU, memory, pods and number of Apps), a LimitRange (defaults and a ceiling per container) and a Role plus RoleBinding that let the team manage Apps, Secrets and ConfigMaps, and read pods, logs and events, in that namespace only. Each team creates its own `registry-credentials` Secret there if it builds from Git.

Two rules to know:

- **Hostnames are global.** An App is served on `<name>.<domain>`, whatever its namespace. If two Apps have the same name, the older one keeps the address and the newer one is not created: `Ready` is False with reason `HostTaken` and the message names the App that owns the host. Rename the newer one.
- **A refused pod is visible.** If the namespace's quota is used up, or a LimitRange refuses the App's limits, the Deployment cannot create pods. `Ready` turns False with reason `ReplicaFailure` and the Deployment's own message (for example `exceeded quota`), even if older pods still run.

Optional admission policies live in `operator/config/policies` (a warning on `latest` image tags and on `maxReplicas` above 10). They start in Warn and Audit mode, and need Kubernetes 1.30 or newer: `kubectl apply -k operator/config/policies`.

### Threat model

This is soft multi-tenancy for teams that trust each other, not isolation for hostile tenants. What is and is not protected:

| Risk | What limits it | What remains |
|---|---|---|
| A Dockerfile runs arbitrary commands during a build | The build is rootless (uid 1000), not privileged, has CPU and memory limits, a timeout and a concurrency limit, and builds only public repositories at an exact commit | The pod runs with seccomp and AppArmor unconfined, which widens the kernel attack surface. A BuildKit or kernel escape would reach the node and every tenant on it |
| A build reaches other things in the cluster | Nothing in the build pod holds a cluster credential | Its egress is open, so it can connect to cluster services and the internet |
| The registry token leaks | It lives in a per-namespace Secret, mounted only into the build container, not into the build steps | The token is a personal access token with broad scopes (see Design Decisions). Anyone who can read the Secret, or compromise BuildKit, gets it. One registry account holds all images, so that token can overwrite any tenant's image |
| The operator is compromised | It reads only the metadata of Secrets and ConfigMaps | Its role can read every Secret and ConfigMap in the cluster (Kubernetes cannot grant metadata-only access) and create workloads everywhere |
| One tenant reaches another's App | RBAC and Secrets are per namespace, NetworkPolicy allows ingress only from Traefik (if the CNI enforces it; k3s does, plain flannel would accept the policies and ignore them), quotas cap usage, the older App keeps a hostname | Egress is open, the kernel is shared, and Pod Security is only warned and audited, because baseline would forbid the build pods' unconfined profiles |
| A tenant claims a domain it does not own | A name must pass the HTTP-01 challenge, so a certificate is issued only if its DNS points at this cluster. Names under the platform domain are refused, and the older App keeps a contested name | Nothing checks who owns a name: the first App to list it holds it. A tenant can sit on a name another team wants, and a name whose DNS already points at the node is served, with a certificate, for whoever claimed it first |
| Code or images are tampered with | Builds use the resolved commit, not the branch name | The BuildKit image is pinned by tag, not digest. Base images in a Dockerfile are not verified, and App images are neither scanned nor signed |
| The Sealed Secrets key leaks | It is made once outside the repo, kept in a file that is not committed, and lives in the cluster as one Secret in `kube-system`, which no tenant Role can read. On the node the Ansible copy is root-only (mode 0600) and the root volume is encrypted | The key opens every sealed file in Git, so whoever reads that Secret, becomes root on the node, or takes the backup file can decrypt all of them. The Secret also sits unencrypted in k3s's datastore and in a manifest file on the node's disk, and the operator's role can read it. If the key is lost, the sealed files cannot be opened and the secrets must be sealed again |
| Secrets at rest | None here | k3s stores Secrets unencrypted in its datastore unless encryption is switched on |

Out of scope: denial of service beyond quotas and the build limits, and tenants who are actively hostile. For those, builds would need to run in a separate cluster or a VM-based sandbox.

### Observability

The playbook installs Prometheus and Grafana into the `monitoring` namespace (`kubectl get pods -n monitoring`; the first start takes a few minutes while the Helm controller pulls the chart). Once the operator is deployed, let Prometheus scrape it and load the dashboard:

```bash
cd operator
make deploy-monitoring
```

This applies `config/monitoring`: a ServiceMonitor for the operator's metrics Service, permission for Prometheus to read `/metrics`, the alert rules, and the Platformator dashboard as a ConfigMap that Grafana's sidecar picks up. Open Grafana through a port-forward:

```bash
kubectl get secret -n monitoring kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80
# http://localhost:3000, user admin, dashboard "Platformator"
```

What the operator exports, on top of the controller-runtime metrics (`controller_runtime_reconcile_total`, `workqueue_depth` and others):

| Metric | Meaning |
|---|---|
| `platformator_apps{ready,reason}` | Apps by the status and reason of their `Ready` condition |
| `platformator_app_builds{state}` | Apps built from Git by the state of their latest build |
| `platformator_builds_total{result}` | Finished builds, `succeeded` or `failed` |
| `platformator_build_duration_seconds` | Histogram of how long finished builds took |

**Alerts.** `config/monitoring/prometheusrule.yaml` defines these rules. There is no Alertmanager, so they are visible, not delivered: in Prometheus under Alerts, and in the dashboard's alerts table.

| Alert | Fires when |
|---|---|
| `PlatformatorOperatorDown` | Prometheus has not scraped the operator for 5 minutes |
| `PlatformatorReconcileErrors` | The App controller returns errors for 10 minutes |
| `PlatformatorAppNotReady` | An App has been not Ready for 10 minutes, rollouts in progress excluded |
| `PlatformatorAppRefusedByQuota` | An App is stuck on `ReplicaFailure` for 5 minutes |
| `PlatformatorBuildFailed` | A Git build failed in the last hour |
| `PlatformatorAppAtMaxReplicas` | An autoscaler has been at its maximum for 15 minutes |

**Logs.** Alloy tails every Pod through the Kubernetes API and ships the lines to Loki, labelled with `namespace`, `pod`, `container` and `app`. A build Pod gets the `app` label of the App it builds, so one query shows an App's logs and its builds' logs. The dashboard's last rows follow the selected App and the operator; the same data is under Explore, data source Loki. Loki keeps its data in the Pod, so logs are gone when the Pod restarts.

To see autoscaling, send load to an App through Traefik (the App's NetworkPolicy only admits Traefik, so a pod in the same namespace cannot do it) and watch the Replicas and CPU panels:

```bash
sed "s/HOST_PLACEHOLDER/hello.$DOMAIN/" examples/loadtest/job.yaml | kubectl apply -f -
kubectl get hpa hello -w
kubectl delete job load   # stops it early; it ends by itself after four minutes
```

### Rolling back

The operator never changes your `spec`, so a rollback is a change you make where the App is defined. If the App manifest lives in Git (with Argo CD, Flux or a CI job applying it), revert the commit that changed the image and let your tooling apply it. That keeps Git the source of truth and leaves a record of the rollback. Without Git in the loop, set `spec.image` back to the previous image with `kubectl patch`.

The operator does not roll back by itself: it would write to `spec`, which Git-managed tooling would change back.

### Validation

The CRD rejects an invalid App when you apply it, so mistakes show up as an error instead of a broken deployment:

| Field | Rule |
|---|---|
| `metadata.name` | A DNS label: lowercase letters, digits and `-`, starts with a letter, at most 63 characters (it becomes the Service name and the hostname) |
| `spec.image` | Required, no whitespace, at most 255 characters |
| `spec.port` | 1-65535 |
| `spec.minReplicas`, `spec.maxReplicas` | At least 1 (a HorizontalPodAutoscaler cannot scale to zero), and `maxReplicas >= minReplicas` |
| `spec.targetCPUUtilizationPercentage` | 1-100 |
| `spec.envFrom` | At most 20 entries, each with exactly one of `secretRef` and `configMapRef` |
| `spec.resources` | CPU and memory requests and limits set, limits not below requests |
| `spec.healthCheck.path`, `.livenessPath` | Start with `/`, no whitespace |

### Releasing the operator image

Day to day, nothing needs tagging:

1. Commit and push to `main`.
2. CI runs lint, unit tests and e2e, and only if all three pass it publishes `<latest version tag>-build.<run number>` (for example `v0.6.0-build.42`). The run summary prints the deploy command. No image appears for a commit whose checks fail.
3. Deploy it: `make deploy-image IMG=ghcr.io/<you>/platformator-operator:v0.6.0-build.42`.

To cut a clean version, tag a commit on `main`. The tag runs the same checks before publishing, so a version never points at code that was not tested:

```bash
git tag v0.7.0
git push origin v0.7.0
```

CI publishes exactly `v0.7.0`, and later builds from `main` become `v0.7.0-build.<N>`. Tags are never overwritten: a second run for the same tag fails on purpose. A release tag rebuilds the image from the same commit, so it has the same code as that commit's `-build.<N>` image but a different digest.

After the image is pushed, CI scans it with Trivy (HIGH and CRITICAL findings that have a fix fail the run) and only then signs it with cosign. A run that fails the scan leaves the image in the registry unsigned, so **deploy only images that verify**. The Go code is checked with `govulncheck` in the lint job, before anything is built.

Each image carries an SBOM and build provenance, attached by BuildKit. Verify the signature, which proves the image was built by this repository's publish workflow, and read the SBOM:

```bash
cosign verify ghcr.io/alexanderbakin/platformator-operator:v0.9.0 \
  --certificate-identity-regexp '^https://github.com/alexanderbakin/platformator/\.github/workflows/publish-image\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

docker buildx imagetools inspect ghcr.io/alexanderbakin/platformator-operator:v0.9.0 --format '{{ json .SBOM }}'
```

Renovate (`renovate.json`) opens pull requests for Go modules, GitHub Actions (pinned by commit), the base image, Terraform providers and the scanner and signer versions in the workflows. It runs on Monday mornings and groups the small updates.

To check what the cluster is running, compare the digest in the run summary with the pod:

```bash
kubectl get pod -n operator-system -o jsonpath='{.items[*].status.containerStatuses[*].imageID}'
```

### Terraform checks

The `Terraform` workflow runs on every change under `terraform/`: `terraform fmt -check`, Checkov (insecure configuration), TFLint with the AWS rule set, and `terraform validate` without a backend, so it needs no AWS credentials. The tools run from pinned versions in their official images, and Renovate raises the updates. The same checks by hand, from `terraform/`:

```bash
terraform fmt -check -diff -recursive
checkov -d . --framework terraform --compact --quiet
tflint --init && tflint
terraform init -backend=false && terraform validate
```

Findings that are accepted are skipped in the code, next to the resource and with the reason (`#checkov:skip=CKV_AWS_260:...`), so a skip cannot hide in a config file. What was fixed instead of skipped: the instance accepts only IMDSv2, its root volume is encrypted, it is EBS-optimized, every security group rule has a description, and the VPC's default security group has no rules. What is skipped, and why: VPC flow logs and detailed monitoring (cost, one node), an IAM role on the instance (nothing on it calls AWS), port 80 open to the world (certificate challenges and the HTTPS redirect) and open egress (the node pulls images and packages).

### Tests

```bash
cd operator
make test       # controller tests against a real API server (envtest)
make lint
make test-e2e   # needs Docker: creates a kind cluster, runs the real operator in it, deletes it
make test-e2e-build   # the one e2e spec that builds an image from Git, see below
```

The first three run in GitHub Actions (`.github/workflows`). The e2e job copies `operator.env.example` into place first, since the real settings file is gitignored.

The Git build spec starts a registry inside the kind cluster, points the operator at it (`REGISTRY_INSECURE=true` makes build Jobs push over plain HTTP and skip the registry cache), creates an App from this repository's `examples/hello` and waits for the image, tagged with the commit, to appear in the registry. It needs unprivileged user namespaces on the host and Docker Hub for the base image, so it has its own workflow (`test-e2e-build.yml`) that does not gate publishing the image.

## Posts

1. [Day 1](https://lnkd.in/p/dk4nhxCg)
2. [Day 2](https://lnkd.in/p/dJvCgHPF)
3. [Day 3](https://lnkd.in/p/d59V8WEu)
4. [Day 4](https://lnkd.in/p/ds2Crscn)
5. [Day 5](https://lnkd.in/p/efpYRzii)
6. [Day 6](https://lnkd.in/p/evxP87nm)
7. [Day 7](https://lnkd.in/p/e3QzRS5H)
8. [Day 8](https://lnkd.in/p/eK4_2NrF)
9. [Day 9](https://lnkd.in/p/ewVprRZs)
10. [Day 10](https://lnkd.in/p/eUdMdwjq)
11. [Day 11](https://lnkd.in/p/emPCUxhH)
12. [Day 12](https://lnkd.in/p/dFq4ZuKn)
13. [Day 13](https://lnkd.in/p/dFyjVT_f)
14. [Day 14](https://lnkd.in/p/dfXV5UEX)
15. [Day 15](https://lnkd.in/p/dMwxi4HZ)
16. [Day 16](https://lnkd.in/p/dhNEFJTX)
17. [Day 17](https://lnkd.in/p/dJNJHKXb)
18. [Day 18](https://lnkd.in/p/dkgW987E)
19. [Day 19](https://lnkd.in/p/deSZWa5n)
20. [Day 20](https://lnkd.in/p/dXHXJynm)
21. [Day 21](https://lnkd.in/p/dSHdGaCN)
22. [Day 22](https://lnkd.in/p/dCXkXxiC)
