output "eip" {
  value       = aws_eip.main.public_ip
  description = "The Elastic IP address of the provisioned instance."
}
