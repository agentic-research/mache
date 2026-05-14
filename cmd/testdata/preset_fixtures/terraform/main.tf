terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type        = string
  description = "AWS region for the deployment."
  default     = "us-east-1"
}

variable "bucket_name" {
  type        = string
  description = "Name of the S3 bucket used for artifact storage."
}

locals {
  common_tags = {
    Project     = "mache-fixture"
    Environment = "test"
  }
}

data "aws_caller_identity" "current" {}

resource "aws_s3_bucket" "artifacts" {
  bucket = var.bucket_name
  tags   = local.common_tags
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  versioning_configuration {
    status = "Enabled"
  }
}

module "logging" {
  source = "./modules/logging"

  bucket_id = aws_s3_bucket.artifacts.id
  tags      = local.common_tags
}

output "bucket_arn" {
  value       = aws_s3_bucket.artifacts.arn
  description = "ARN of the artifact bucket."
}

output "account_id" {
  value = data.aws_caller_identity.current.account_id
}
