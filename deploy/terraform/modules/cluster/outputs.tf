output "cluster_name" { value = aws_eks_cluster.main.name }
output "cluster_endpoint" { value = aws_eks_cluster.main.endpoint }
output "service_security_group_id" { value = aws_security_group.nodes["services"].id }
output "generator_security_group_id" { value = aws_security_group.nodes["load-generator"].id }
output "controller_role_arn" { value = aws_iam_role.controller.arn }
