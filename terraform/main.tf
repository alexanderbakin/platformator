# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/vpc
resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/subnet
resource "aws_subnet" "main" {
  vpc_id     = aws_vpc.main.id
  cidr_block = var.subnet_cidr
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/internet_gateway
resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/security_group
resource "aws_security_group" "main" {
  name        = "platformator"
  description = "Allow inbound SSH, HTTP(s) and Kubernetes API"
  vpc_id      = aws_vpc.main.id
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/vpc_security_group_ingress_rule
resource "aws_vpc_security_group_ingress_rule" "allow_ssh" {
  for_each          = toset(nonsensitive(var.developer_cidrs))
  security_group_id = aws_security_group.main.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
}

resource "aws_vpc_security_group_ingress_rule" "allow_http" {
  security_group_id = aws_security_group.main.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
}

resource "aws_vpc_security_group_ingress_rule" "allow_https" {
  security_group_id = aws_security_group.main.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_ingress_rule" "allow_kubernetes_api" {
  for_each          = toset(nonsensitive(var.developer_cidrs))
  security_group_id = aws_security_group.main.id
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 6443
  to_port           = 6443
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/vpc_security_group_egress_rule
resource "aws_vpc_security_group_egress_rule" "allow_all" {
  security_group_id = aws_security_group.main.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1" # semantically equivalent to all ports
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route_table
resource "aws_route_table" "main" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route_table_association
resource "aws_route_table_association" "main" {
  subnet_id      = aws_subnet.main.id
  route_table_id = aws_route_table.main.id
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/key_pair
resource "aws_key_pair" "developer" {
  key_name   = "platformator-developer"
  public_key = var.developer_key
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/instance
resource "aws_instance" "main" {
  ami                    = data.aws_ami.amazon.id
  instance_type          = var.instance_type
  vpc_security_group_ids = [aws_security_group.main.id]
  subnet_id              = aws_subnet.main.id
  key_name               = aws_key_pair.developer.key_name
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/eip
resource "aws_eip" "main" {
  instance = aws_instance.main.id
  domain   = "vpc"
}

# https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route53_record
resource "aws_route53_record" "apex" {
  zone_id = data.aws_route53_zone.main.zone_id
  name    = var.domain
  type    = "A"
  ttl     = 60
  records = [aws_eip.main.public_ip]
}

resource "aws_route53_record" "wildcard" {
  zone_id = data.aws_route53_zone.main.zone_id
  name    = "*.${var.domain}"
  type    = "A"
  ttl     = 60
  records = [aws_eip.main.public_ip]
}
