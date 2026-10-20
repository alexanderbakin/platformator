# platformator

A tiny PaaS built as a 30-day challenge. A Kubernetes Operator (CRDs + Controller) for deploying applications that autoscale on AWS.

## Status

Day 16 of 30.

Single-node k3s cluster on AWS, reachable over a real domain, serving trusted Let's Encrypt certificates via cert-manager. Apps are deployed through an `App` CRD and a controller (Kubebuilder, Go) that creates and keeps in sync a Deployment, Service, Ingress and HorizontalPodAutoscaler for each App.

What the operator does today:

- **Per-App spec:** image, port, optional `healthCheck` (`path`, `livenessPath`), optional `env` and `envFrom` (Secrets and ConfigMaps), required CPU/memory requests and limits (checked by a CEL rule on the CRD), and min/max replicas plus a target CPU utilization that become an HPA. The CRD rejects bad Apps up front, see [Validation](#validation).
- **Health probes:** every App gets a readiness probe, a TCP connect on its port, so a pod whose process isn't listening is never counted as available. With `healthCheck.path` (for example `/healthz`, must start with `/`) readiness becomes an HTTP request on that path. It may check dependencies such as a database: a failing readiness probe only takes the pod out of the Service. Liveness is a separate, optional `healthCheck.livenessPath` (for example `/livez`) and is never derived from `path`, because a failing liveness probe restarts the container, and a liveness check that depends on a database would restart every pod at once during a database outage. There is no default liveness probe, because a wrong guess would restart healthy apps.
- **Public URL and TLS status:** `status.url` holds the address the App is served on, and the `CertificateReady` condition follows the cert-manager Certificate for it (`True` once issued, `False` with cert-manager's own reason while pending, `Unknown` if cert-manager isn't installed).
- **Status conditions:** `Reconciled` means every API call in the last pass succeeded. `Ready` means the Deployment's rollout is complete and Available and, if cert-manager is installed, the TLS certificate is issued. `Ready` stays False while a rollout is in progress or stuck, even if old pods are still serving, and while the certificate is pending.
- **Events:** each App gets Kubernetes Events when its `Ready` or `Reconciled` condition changes and when its certificate is issued, so `kubectl describe app <name>` shows the history (rollout in progress, certificate pending, ready, failures). Events are sent on changes only, not on every reconcile pass.
- **Builds from Git:** instead of an `image`, an App can name a public repository. The operator watches the branch, builds the Dockerfile in the cluster with BuildKit, pushes to GHCR and rolls the Deployment to the new image. See [Building from Git](#building-from-git).
- **In-cluster deployment:** runs from an image on GHCR. The base domain comes from a gitignored `operator.env` through a Kustomize-generated ConfigMap, so it never lands in git.
- **Tests:** envtest-based tests for the controller, a kind-based e2e suite that runs the real operator as its real ServiceAccount, and GitHub Actions running lint, unit tests and e2e on every push.

## Architecture

Three layers, each built by a different tool:

```
Terraform  ->  AWS: EC2 instance, Route53 zone for the subdomain, state bucket
Ansible    ->  the node: k3s (with Traefik), cert-manager, a Let's Encrypt ClusterIssuer
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

**Polling Git instead of webhooks.** A webhook needs a public endpoint on the operator, a secret and per-provider setup. Polling works with any HTTPS Git host and nothing exposed. Cost: up to a minute of delay and one request per App per minute.

**One node, on purpose.** The target is a single k3s node on AWS: cheap, easy to rebuild from Terraform and Ansible, and enough to show the platform. Cost: no high availability. The operator itself would not need to change to run on more nodes.

**CI gates the image.** The operator image is published only after lint, unit tests and e2e on kind pass, so what lands on GHCR has been tested.

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

read -p "Email for Let's Encrypt expiry/revocation notices: " ACME_EMAIL
export ACME_EMAIL
```

These stay exported for the rest of this walkthrough - Terraform's backend init, the Route53 lookup, the Ansible dynamic inventory, and the k3s/cert-manager config all read them back out of the environment rather than a committed file, since they're specific to your setup.

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

By default both k3s and cert-manager install whatever's currently latest. To pin either for a reproducible provision:

```bash
ansible-playbook playbook.yml -e k3s_version=v1.32.1+k3s1 -e cert_manager_version=v1.21.2
```

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

Every key of the Secret or ConfigMap becomes an environment variable. Each entry sets exactly one of `secretRef` and `configMapRef`. The objects must exist in the App's namespace: until they do, the pod cannot start (`CreateContainerConfigError`) and the App stays not Ready, then it starts by itself once they exist. The operator never reads the Secret, only the pod does.

Pods read these values when they start. After changing a Secret or ConfigMap, delete the pods (`kubectl delete pod -l app=<name>`) so the new ones pick up the change.

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
- `kubectl get app -o wide` shows the built `COMMIT`, the `Built` condition has the state, and `status.build.jobName` points at the Job whose logs hold the build output.

Setup, once:

```sh
# 1. Where images go: add to config/default/operator.env (not committed)
REGISTRY=ghcr.io/<your-github-name>

# 2. Credentials to push and pull (a token with write:packages), in each App's namespace
kubectl create secret docker-registry registry-credentials \
  --docker-server=ghcr.io --docker-username=<your-github-name> --docker-password=<token>
```

Limits for now: public repositories only, one Dockerfile build per commit, and the BuildKit Job runs rootless with seccomp and AppArmor unconfined, which the node must allow.

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

To check what the cluster is running, compare the digest in the run summary with the pod:

```bash
kubectl get pod -n operator-system -o jsonpath='{.items[*].status.containerStatuses[*].imageID}'
```

### Tests

```bash
cd operator
make test       # controller tests against a real API server (envtest)
make lint
make test-e2e   # needs Docker: creates a kind cluster, runs the real operator in it, deletes it
```

The same three run in GitHub Actions (`.github/workflows`). The e2e job copies `operator.env.example` into place first, since the real settings file is gitignored.

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
