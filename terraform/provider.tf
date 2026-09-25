provider "aws" {
  region = var.region
  default_tags {
    tags = {
      Project   = "platformator"
      ManagedBy = "Terraform"
    }
  }
}
