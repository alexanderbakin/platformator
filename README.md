# platformator

A tiny PaaS built as a 30-day challenge. A Kubernetes Operator (CRDs + Controller) for deploying applications that autoscale on AWS.

## Status

Day 2 of 30.

Single-node k3s cluster running on AWS, reachable over a real domain (no trusted TLS yet).

## Architecture

WIP.

## Design Decisions

WIP.

## Getting started

### Bootstrap the state bucket

The S3 backend needs a bucket to exist before `terraform init` can use it:

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

Terraform looks up a hosted zone by name at apply time rather than creating one itself, so a zone has to exist first. If you don't already have one, create a hosted zone scoped to the `paas` subdomain specifically - not your whole domain - so you're not handing DNS for your entire domain over to Route53:

```bash
read -p "Hosted zone name, e.g. paas.example.com: " HOSTED_ZONE_NAME

aws route53 create-hosted-zone \
  --name "$HOSTED_ZONE_NAME" \
  --caller-reference "$(date +%s)"
```

The response includes a `DelegationSet.NameServers` list (four hostnames) and the zone's `Id`. At your domain's registrar, add those four as `NS` records for the `paas` host - this delegates just that subtree to Route53, leaving the rest of your domain's DNS untouched. Delegation typically settles within an hour, much faster than a full domain nameserver change, since it's an ordinary record addition rather than a change at the registry level.

If you already manage this domain (or this subdomain) in Route53 - say, from another project - skip creation and just look up the existing zone instead:

```bash
aws route53 list-hosted-zones-by-name --dns-name "<your-domain>."
```

Either way, add the zone name and the subdomain you want to serve the platform on to `terraform.tfvars`:

```hcl
hosted_zone_name = "<your-domain>."   # must end in a dot
domain           = "paas.<your-domain>"
```

### Apply

```bash
terraform apply
```

This provisions the VPC, the EC2 instance, its Elastic IP, and the `paas.<your-domain>` + wildcard DNS records pointing at that IP.

### Configure the node with Ansible

Install dependencies:

```bash
cd ../ansible
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
ansible-galaxy collection install -r requirements.yml
```

The dynamic inventory and the k3s config template both read from environment variables rather than committed files, since these are specific to your setup:

```bash
export AWS_DEFAULT_REGION="<same region as terraform.tfvars>"
export DOMAIN="paas.<your-domain>"
```

Run the playbook:

```bash
ansible-playbook playbook.yml
```

This installs and starts k3s, and fetches its kubeconfig back to `ansible/kubeconfig`, already patched to point at the node's public IP instead of `127.0.0.1`.

### Use kubectl

```bash
export KUBECONFIG=./ansible/kubeconfig
kubectl get nodes
```

## Posts

WIP.
