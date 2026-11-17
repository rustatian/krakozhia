terraform {
  required_version = ">= 1.16"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.73"
    }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  description = "AWS Region of the vector bucket and of the embedding model."
  type        = string
}

variable "bucket_name" {
  description = "Name of the S3 vector bucket."
  type        = string
}

resource "aws_s3vectors_vector_bucket" "memory" {
  vector_bucket_name = var.bucket_name
}

resource "aws_iam_user" "krakozhia" {
  name = "krakozhia"
}

data "aws_iam_policy_document" "krakozhia" {
  statement {
    actions = [
      "s3vectors:PutVectors",
      "s3vectors:QueryVectors",
      "s3vectors:GetVectors",
      "s3vectors:DeleteVectors",
    ]
    resources = ["${aws_s3vectors_vector_bucket.memory.vector_bucket_arn}/index/*"]
  }

  statement {
    actions   = ["bedrock:InvokeModel"]
    resources = ["arn:aws:bedrock:${var.region}::foundation-model/amazon.titan-embed-text-v2:0"]
  }
}

resource "aws_iam_user_policy" "krakozhia" {
  name   = "krakozhia"
  user   = aws_iam_user.krakozhia.name
  policy = data.aws_iam_policy_document.krakozhia.json
}
