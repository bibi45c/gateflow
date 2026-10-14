output "vpc_id" { value = module.network.vpc_id }
output "public_subnet_ids" { value = module.network.public_subnet_ids }
output "private_subnet_ids" { value = module.network.private_subnet_ids }
output "cluster_name" { value = module.cluster.cluster_name }
output "cluster_endpoint" { value = module.cluster.cluster_endpoint }
output "service_security_group_id" { value = module.cluster.service_security_group_id }
output "generator_security_group_id" { value = module.cluster.generator_security_group_id }
output "controller_role_arn" { value = module.cluster.controller_role_arn }
