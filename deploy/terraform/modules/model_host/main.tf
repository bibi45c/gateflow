data "aws_ssm_parameter" "ami" {
  count = var.ami_id == "" ? 1 : 0
  name  = var.ami_ssm_parameter
}
locals {
  ami_id = var.ami_id != "" ? var.ami_id : data.aws_ssm_parameter.ami[0].insecure_value
  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    region                 = var.region
    image                  = var.vllm_image
    model                  = var.model
    revision               = var.model_revision
    served_name            = var.served_model_name
    port                   = var.port
    max_num_seqs           = var.max_num_seqs
    max_model_len          = var.max_model_len
    gpu_memory_utilization = var.gpu_memory_utilization
    extra_args             = var.extra_args
    api_key_ssm_parameter  = var.api_key_ssm_parameter
  })
}

resource "aws_security_group" "model" {
  name_prefix = "${var.name}-model-"
  description = "vLLM model host: the vLLM port from the cluster nodes only"
  vpc_id      = var.vpc_id
  tags        = merge(var.tags, { Name = "${var.name}-model" })
}
resource "aws_vpc_security_group_ingress_rule" "vllm" {
  for_each                     = var.client_security_group_ids
  security_group_id            = aws_security_group.model.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = var.port
  to_port                      = var.port
  description                  = "vLLM from ${each.key} nodes"
  tags                         = var.tags
}
# Image and model downloads go out through the NAT gateway.
resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.model.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  tags              = var.tags
}

# Session Manager only: no SSH key and no port 22.
resource "aws_iam_role" "model" {
  name_prefix = "${var.name}-model-"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
  tags = var.tags
}
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.model.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
resource "aws_iam_role_policy" "api_key" {
  count = var.api_key_ssm_parameter == "" ? 0 : 1
  name  = "read-vllm-api-key"
  role  = aws_iam_role.model.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "ssm:GetParameter"
      Resource = "arn:aws:ssm:${var.region}:${var.account_id}:parameter/${trimprefix(var.api_key_ssm_parameter, "/")}"
    }]
  })
}
resource "aws_iam_instance_profile" "model" {
  name_prefix = "${var.name}-model-"
  role        = aws_iam_role.model.name
  tags        = var.tags
}

resource "aws_instance" "model" {
  ami                         = local.ami_id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  vpc_security_group_ids      = [aws_security_group.model.id]
  iam_instance_profile        = aws_iam_instance_profile.model.name
  associate_public_ip_address = false
  user_data                   = local.user_data
  user_data_replace_on_change = true
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  root_block_device {
    volume_size           = var.disk_gb
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
    tags                  = merge(var.tags, { Name = "${var.name}-model" })
  }
  tags = merge(var.tags, { Name = "${var.name}-model" })
  lifecycle {
    # A newer AMI behind the SSM parameter must not replace a running host; set ami_id to change it.
    ignore_changes = [ami]
  }
}
