resource "aws_iam_role" "cluster" {
  name = "${var.name}-cluster"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "eks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
  tags = var.tags
}
resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}
resource "aws_iam_role" "nodes" {
  name = "${var.name}-nodes"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
  tags = var.tags
}
resource "aws_iam_role_policy_attachment" "nodes" {
  for_each   = toset(["AmazonEKSWorkerNodePolicy", "AmazonEKS_CNI_Policy", "AmazonEC2ContainerRegistryPullOnly"])
  role       = aws_iam_role.nodes.name
  policy_arn = "arn:aws:iam::aws:policy/${each.value}"
}
locals {
  oidc_issuer = aws_eks_cluster.main.identity[0].oidc[0].issuer
  oidc_host   = trimprefix(local.oidc_issuer, "https://")
}
data "tls_certificate" "oidc" { url = local.oidc_issuer }
resource "aws_iam_openid_connect_provider" "cluster" {
  url             = local.oidc_issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]
  tags            = var.tags
}
resource "aws_iam_role" "controller" {
  name = "${var.name}-load-balancer-controller"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.cluster.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = { StringEquals = {
        "${local.oidc_host}:aud" = "sts.amazonaws.com"
        "${local.oidc_host}:sub" = "system:serviceaccount:kube-system:aws-load-balancer-controller"
      } }
    }]
  })
  tags = var.tags
}
# Official v3.6.0 policy; see README for source and SHA256.
resource "aws_iam_policy" "controller" {
  name   = "${var.name}-load-balancer-controller"
  policy = file("${path.module}/policies/load-balancer-controller.json")
  tags   = var.tags
}
resource "aws_iam_role_policy_attachment" "controller" {
  role       = aws_iam_role.controller.name
  policy_arn = aws_iam_policy.controller.arn
}
