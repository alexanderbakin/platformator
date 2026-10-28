# Lint rules for the Terraform code. Run by the "Terraform" workflow and by hand:
# see the README, "Terraform checks".
plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

plugin "aws" {
  enabled = true
  version = "0.30.0"
  source  = "github.com/terraform-linters/tflint-ruleset-aws"
}
