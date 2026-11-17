# Workload identity for the pods that back up and restore databases: the same
# mechanism as IRSA on EKS, built by hand for k3s. A pod gets a short-lived
# token signed by the cluster, and AWS trusts the cluster's public keys, which
# are published in a bucket. No pod holds an access key, and the node has no
# instance profile for a pod to borrow.
#
# AWS reads two files over HTTPS to check a token: the discovery document and
# the keys. The discovery document is the same for every cluster and is made
# here. The keys belong to one cluster and are published by the Ansible
# playbook after k3s starts, so a rebuilt cluster publishes its own.

locals {
  oidc_host   = "${aws_s3_bucket.oidc.bucket}.s3.${var.region}.amazonaws.com"
  oidc_issuer = "https://${local.oidc_host}"
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket
resource "aws_s3_bucket" "oidc" {
  #checkov:skip=CKV_AWS_18:The two files are public by design and nobody's data is read through them
  #checkov:skip=CKV_AWS_21:The keys are replaced by every new cluster, old versions are of no use
  #checkov:skip=CKV_AWS_144:Cross-region replication of a file the playbook can publish again is not needed
  #checkov:skip=CKV_AWS_145:The files are public, so encrypting them with a key adds nothing
  #checkov:skip=CKV2_AWS_6:The public access block lets a bucket policy through, for the two discovery files
  #checkov:skip=CKV2_AWS_61:Two small files, nothing to expire
  #checkov:skip=CKV2_AWS_62:Nothing listens to events of this bucket
  bucket        = "platformator-oidc-${data.aws_caller_identity.current.account_id}"
  force_destroy = true
}

# Public access is on for this bucket only, and the policy below narrows it to
# the two files. ACLs stay blocked.
# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket_public_access_block
resource "aws_s3_bucket_public_access_block" "oidc" {
  #checkov:skip=CKV_AWS_54:The bucket policy has to be public, for the two discovery files
  #checkov:skip=CKV_AWS_56:The bucket policy has to be public, for the two discovery files
  bucket                  = aws_s3_bucket.oidc.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = false
  restrict_public_buckets = false
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket_policy
resource "aws_s3_bucket_policy" "oidc" {
  #checkov:skip=CKV_AWS_70:Anyone may read the two discovery files, and nothing else
  bucket = aws_s3_bucket.oidc.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "PublicReadOfTheDiscoveryFiles"
      Effect    = "Allow"
      Principal = "*"
      Action    = "s3:GetObject"
      Resource = [
        "${aws_s3_bucket.oidc.arn}/.well-known/openid-configuration",
        "${aws_s3_bucket.oidc.arn}/keys.json",
      ]
    }]
  })
  depends_on = [aws_s3_bucket_public_access_block.oidc]
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_object
resource "aws_s3_object" "discovery" {
  bucket       = aws_s3_bucket.oidc.id
  key          = ".well-known/openid-configuration"
  content_type = "application/json"
  content = jsonencode({
    issuer                                = local.oidc_issuer
    jwks_uri                              = "${local.oidc_issuer}/keys.json"
    authorization_endpoint                = "urn:kubernetes:programmatic_authorization"
    response_types_supported              = ["id_token"]
    subject_types_supported               = ["public"]
    id_token_signing_alg_values_supported = ["RS256"]
    claims_supported                      = ["sub", "iss"]
  })
}

# AWS checks the certificate of the bucket's address against its own list of
# trusted authorities, so no thumbprint is needed.
# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_openid_connect_provider
resource "aws_iam_openid_connect_provider" "cluster" {
  url            = local.oidc_issuer
  client_id_list = ["sts.amazonaws.com"]
}

# The role the backup pods assume. Only a token whose subject is the
# platformator-backup ServiceAccount, in any namespace, is accepted, and only
# one made for AWS (the audience the operator projects).
# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role
resource "aws_iam_role" "backup" {
  name = "platformator-backup"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.cluster.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = { "${local.oidc_host}:aud" = "sts.amazonaws.com" }
        StringLike   = { "${local.oidc_host}:sub" = "system:serviceaccount:*:platformator-backup" }
      }
    }]
  })
}

# Upload and download in the backup bucket, nothing else: no listing, no delete.
# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role_policy
resource "aws_iam_role_policy" "backup" {
  name = "backup-bucket"
  role = aws_iam_role.backup.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:PutObject", "s3:GetObject"]
      Resource = "${aws_s3_bucket.backups.arn}/*"
    }]
  })
}
