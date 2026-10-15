resource "aws_security_group" "control" {
  name_prefix = "${var.name}-control-"
  description = "EKS control-plane communication"
  vpc_id      = var.vpc_id
  tags        = var.tags
}
resource "aws_security_group" "nodes" {
  for_each    = local.groups
  name_prefix = "${var.name}-${each.key}-"
  description = "Private ${each.key} nodes"
  vpc_id      = var.vpc_id
  tags        = var.tags
}
resource "aws_vpc_security_group_egress_rule" "control" {
  security_group_id = aws_security_group.control.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  tags              = var.tags
}
resource "aws_vpc_security_group_egress_rule" "nodes" {
  for_each          = local.groups
  security_group_id = aws_security_group.nodes[each.key].id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  tags              = var.tags
}
resource "aws_vpc_security_group_ingress_rule" "api" {
  for_each                     = local.groups
  security_group_id            = aws_security_group.control.id
  referenced_security_group_id = aws_security_group.nodes[each.key].id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  tags                         = var.tags
}
resource "aws_vpc_security_group_ingress_rule" "control_nodes" {
  for_each                     = local.groups
  security_group_id            = aws_security_group.nodes[each.key].id
  referenced_security_group_id = aws_security_group.control.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 65535
  tags                         = var.tags
}
locals {
  peers = { for p in setproduct(keys(local.groups), keys(local.groups), ["tcp", "udp"]) : join("-", p) => p }
}
resource "aws_vpc_security_group_ingress_rule" "peers" {
  for_each                     = local.peers
  security_group_id            = aws_security_group.nodes[each.value[0]].id
  referenced_security_group_id = aws_security_group.nodes[each.value[1]].id
  ip_protocol                  = each.value[2]
  from_port                    = 0
  to_port                      = 65535
  tags                         = var.tags
}
