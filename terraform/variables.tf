variable "region" {
  type        = string
  description = "AWS region to provision resource in"
}

variable "instance_type" {
  type        = string
  description = "Type of the EC2 instance"
  default     = "t3.micro"
}

variable "vpc_cidr" {
  type        = string
  description = "CIDR range of the VPC"
  default     = "10.0.0.0/16"
}

variable "subnet_cidr" {
  type        = string
  description = "CIDR range of the subnet"
  default     = "10.0.0.0/24"
}

variable "developer_cidrs" {
  type        = list(string)
  description = "CIDR ranges allowed to access the instance via SSH and Kubernetes API"
  sensitive   = true
}

variable "developer_key" {
  type        = string
  description = "SSH public key to access the instance via SSH"
  sensitive   = true
}

variable "hosted_zone_name" {
  type        = string
  description = "Root domain's Route53 hosted zone name (must end in a dot)"
}

variable "domain" {
  type        = string
  description = "Base domain the platform is served on (e.g. paas.alexanderbakin.com)"
}
