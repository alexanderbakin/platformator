# platformator

A tiny PaaS built as a 30-day challenge. A Kubernetes Operator (CRDs + Controller) for deploying applications that autoscale on AWS.

## Status

Day 3 of 30.

Single-node k3s cluster running on AWS, reachable over a real domain, serving trusted Let's Encrypt certificates via cert-manager.

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

### Deploy the example app

`examples/hello/` is a minimal Deployment/Service/Ingress used to prove the ingress and TLS path work - not part of the platform itself, and not applied automatically by Ansible. Its Ingress templates in `$DOMAIN` via `envsubst`:

```bash
envsubst < examples/hello/ingress.yaml | kubectl apply -f examples/hello/deployment.yaml -f examples/hello/service.yaml -f -
kubectl get certificate hello-tls -w   # wait for READY=True
curl "https://hello.$DOMAIN/"
```

No `-k`, no warnings - a real Let's Encrypt cert. `kubectl describe certificate hello-tls` shows the renewal window (cert-manager renews automatically well before the 90-day expiry).

## Posts

WIP.
