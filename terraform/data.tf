data "aws_ami" "amazon" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
  filter {
    name   = "name"
    values = ["al2023-ami-2023*"]
  }
}

data "aws_route53_zone" "main" {
  name         = var.hosted_zone_name
  private_zone = false
}
