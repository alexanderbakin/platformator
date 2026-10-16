# platformator

A tiny PaaS built as a 30-day challenge. A Kubernetes Operator (CRDs + Controller) for deploying applications that autoscale on AWS.

## Status

Day 13 of 30.

Single-node k3s cluster on AWS, reachable over a real domain, serving trusted Let's Encrypt certificates via cert-manager. Apps are deployed through an `App` CRD and a controller (Kubebuilder, Go) that creates and keeps in sync a Deployment, Service, Ingress and HorizontalPodAutoscaler for each App.

What the operator does today:

- **Per-App spec:** image, port, optional `healthCheck.path`, optional env vars, required CPU/memory requests and limits (checked by a CEL rule on the CRD), and min/max replicas plus a target CPU utilization that become an HPA.
- **Health probes:** every App gets a readiness probe, a TCP connect on its port, so a pod whose process isn't listening is never counted as available. With `healthCheck.path` (for example `/healthz`, must start with `/`) readiness becomes an HTTP request on that path and a liveness probe is added, so a hung process gets restarted. There is no default liveness probe, because a wrong guess would restart healthy apps.
- **Public URL and TLS status:** `status.url` holds the address the App is served on, and the `CertificateReady` condition follows the cert-manager Certificate for it (`True` once issued, `False` with cert-manager's own reason while pending, `Unknown` if cert-manager isn't installed).
- **Status conditions:** `Reconciled` means every API call in the last pass succeeded. `Ready` means the Deployment's rollout is complete and Available and, if cert-manager is installed, the TLS certificate is issued. `Ready` stays False while a rollout is in progress or stuck, even if old pods are still serving, and while the certificate is pending.
- **In-cluster deployment:** runs from an image on GHCR. The base domain comes from a gitignored `domain.env` through a Kustomize-generated ConfigMap, so it never lands in git.
- **Tests:** envtest-based tests for the controller, a kind-based e2e suite that runs the real operator as its real ServiceAccount, and GitHub Actions running lint, unit tests and e2e on every push.

## Architecture

WIP.

## Design Decisions

WIP.

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
cp config/default/domain.env.example config/default/domain.env   # then set DOMAIN=<your domain>

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

The columns show the image, the public URL, the two conditions, the reason for `Ready`, and the TLS status (the `CertificateReady` condition). For example, an App whose image can't be pulled stays `Reconciled=True` (the API calls worked) but `Ready=False`:

| Ready | Reason | Meaning |
|---|---|---|
| `True` | `DeploymentAvailable` | Rollout complete and Available |
| `False` | `DeploymentUnavailable` | The Deployment has no minimum availability, for example the pod can't start or fails its readiness probe |
| `False` | `RolloutInProgress` | A rollout is still going or stuck, even if old pods still serve |
| `False` | `CertificateNotReady` | The pods are fine but the TLS certificate isn't issued yet, see the message and the `CertificateReady` condition |
| `False` | `ProgressDeadlineExceeded` | The rollout made no progress within the Deployment's deadline |
| `False` | `ReconcileError` | The last reconcile pass failed, see the `Reconciled` condition |

The TLS column and `kubectl describe app` show the certificate state:

| CertificateReady | Reason | Meaning |
|---|---|---|
| `True` | `CertificateIssued` | cert-manager issued the certificate |
| `False` | `CertificatePending` | cert-manager has not created the Certificate or reported its state yet |
| `False` | cert-manager's reason (for example `DoesNotExist`, `Issuing`) | Issuance is in progress or failing, see the message |
| `Unknown` | `CertManagerNotInstalled` | The Certificate CRD isn't installed, so there is nothing to check |
| `Unknown` | `CertificateCheckFailed` | The operator could not read the Certificate, see the message |

An App whose certificate isn't issued yet is checked again every 10 seconds.

### Releasing the operator image

Day to day, nothing needs tagging:

1. Commit and push to `main`.
2. CI runs lint, unit tests and e2e, and publishes `<latest version tag>-build.<run number>` (for example `v0.6.0-build.42`). The run summary prints the deploy command.
3. Deploy it: `make deploy-image IMG=ghcr.io/<you>/platformator-operator:v0.6.0-build.42`.

To cut a clean version, tag a commit on `main` after its checks are green, so a version number never points at an untested commit:

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

The same three run in GitHub Actions (`.github/workflows`). The e2e job copies `domain.env.example` into place first, since the real domain file is gitignored.

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
