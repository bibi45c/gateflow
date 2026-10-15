locals {
  groups = {
    services       = { instance = var.service_instance_type, count = var.service_node_count }
    load-generator = { instance = var.generator_instance_type, count = var.generator_node_count }
  }
}
resource "aws_eks_cluster" "main" {
  name     = var.name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version
  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }
  upgrade_policy { support_type = "STANDARD" }
  vpc_config {
    subnet_ids              = var.private_subnet_ids
    security_group_ids      = [aws_security_group.control.id]
    endpoint_private_access = true
    endpoint_public_access  = true
    public_access_cidrs     = var.operator_cidrs
  }
  tags       = var.tags
  depends_on = [aws_iam_role_policy_attachment.cluster]
}
resource "aws_eks_access_entry" "operator" {
  cluster_name  = aws_eks_cluster.main.name
  principal_arn = var.operator_principal_arn
  type          = "STANDARD"
  tags          = var.tags
}
resource "aws_eks_access_policy_association" "operator" {
  cluster_name  = aws_eks_cluster.main.name
  principal_arn = aws_eks_access_entry.operator.principal_arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
  access_scope { type = "cluster" }
}
resource "aws_launch_template" "nodes" {
  for_each               = local.groups
  name_prefix            = "${var.name}-${each.key}-"
  vpc_security_group_ids = [aws_security_group.nodes[each.key].id]
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size           = var.node_disk_gb
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }
  dynamic "tag_specifications" {
    for_each = toset(["instance", "volume", "network-interface"])
    content {
      resource_type = tag_specifications.value
      tags          = merge(var.tags, { Name = "${var.name}-${each.key}" })
    }
  }
  tags = var.tags
}
resource "aws_eks_node_group" "workload" {
  for_each        = local.groups
  cluster_name    = aws_eks_cluster.main.name
  node_group_name = each.key
  node_role_arn   = aws_iam_role.nodes.arn
  subnet_ids      = var.private_subnet_ids
  ami_type        = "AL2023_x86_64_STANDARD"
  capacity_type   = "ON_DEMAND"
  instance_types  = [each.value.instance]
  version         = var.kubernetes_version
  launch_template {
    id      = aws_launch_template.nodes[each.key].id
    version = aws_launch_template.nodes[each.key].latest_version
  }
  scaling_config {
    min_size     = each.value.count
    max_size     = each.value.count
    desired_size = each.value.count
  }
  labels = { "gateflow.io/workload" = each.key }
  dynamic "taint" {
    for_each = each.key == "load-generator" ? [1] : []
    content {
      key    = "gateflow.io/workload"
      value  = "load-generator"
      effect = "NO_SCHEDULE"
    }
  }
  tags       = var.tags
  depends_on = [aws_iam_role_policy_attachment.nodes]
}
