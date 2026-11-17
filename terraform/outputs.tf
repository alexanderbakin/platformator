output "eip" {
  value       = aws_eip.main.public_ip
  description = "The Elastic IP address of the provisioned instance."
}

output "service_account_issuer" {
  value       = local.oidc_issuer
  description = "Issuer the cluster signs ServiceAccount tokens with. Export it as SERVICE_ACCOUNT_ISSUER before running Ansible."
}

output "oidc_bucket" {
  value       = aws_s3_bucket.oidc.bucket
  description = "Bucket that publishes the cluster's public keys. Export it as OIDC_BUCKET before running Ansible."
}

output "backup_bucket" {
  value       = aws_s3_bucket.backups.bucket
  description = "Bucket that holds database dumps. Put it in operator.env as BACKUP_BUCKET."
}

output "backup_role_arn" {
  value       = aws_iam_role.backup.arn
  description = "Role the backup pods assume. Put it in operator.env as BACKUP_ROLE_ARN."
}
