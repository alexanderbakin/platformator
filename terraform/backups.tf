# Where database dumps go: s3://<bucket>/<namespace>/<app>/<timestamp>.dump.
# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket
resource "aws_s3_bucket" "backups" {
  #checkov:skip=CKV_AWS_18:Access logs cost money, and CloudTrail records who reads the dumps if it is on
  #checkov:skip=CKV_AWS_144:A second region doubles the cost, and the dumps are a copy of data that lives on the node
  #checkov:skip=CKV_AWS_145:Dumps are encrypted with the bucket's own AWS-managed key, not a customer key that costs per request
  #checkov:skip=CKV2_AWS_62:Nothing listens to events of this bucket
  bucket        = "platformator-backups-${data.aws_caller_identity.current.account_id}"
  force_destroy = var.backup_force_destroy
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket_public_access_block
resource "aws_s3_bucket_public_access_block" "backups" {
  bucket                  = aws_s3_bucket.backups.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket_versioning
resource "aws_s3_bucket_versioning" "backups" {
  bucket = aws_s3_bucket.backups.id
  versioning_configuration {
    status = "Enabled"
  }
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket_server_side_encryption_configuration
resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Dumps expire after backup_retention_days. The role cannot delete, so this is
# the only thing that does.
# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket_lifecycle_configuration
resource "aws_s3_bucket_lifecycle_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    id     = "expire-dumps"
    status = "Enabled"
    filter {}
    expiration {
      days = var.backup_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 1
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
  depends_on = [aws_s3_bucket_versioning.backups]
}
