terraform {
  required_version = ">= 1.16.3"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.65"
    }
  }

  backend "s3" {
    key          = "terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}
