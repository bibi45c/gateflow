module "network" {
  source             = "./modules/network"
  name               = "gateflow-${var.environment}"
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones
  tags               = local.tags
}

module "cluster" {
  source                  = "./modules/cluster"
  name                    = "gateflow-${var.environment}"
  vpc_id                  = module.network.vpc_id
  private_subnet_ids      = module.network.private_subnet_ids
  operator_principal_arn  = var.operator_principal_arn
  operator_cidrs          = var.operator_cidrs
  kubernetes_version      = var.kubernetes_version
  service_instance_type   = var.service_instance_type
  service_node_count      = var.service_node_count
  generator_instance_type = var.generator_instance_type
  generator_node_count    = var.generator_node_count
  node_disk_gb            = var.node_disk_gb
  tags                    = local.tags
  depends_on              = [module.network]
}

module "database" {
  source                    = "./modules/database"
  name                      = "gateflow-${var.environment}"
  vpc_id                    = module.network.vpc_id
  private_subnet_ids        = module.network.private_subnet_ids
  service_security_group_id = module.cluster.service_security_group_id
  instance_class            = var.database_instance_class
  allocated_storage         = var.database_storage_gb
  engine_version            = var.database_engine_version
  deletion_protection       = var.database_deletion_protection
  skip_final_snapshot       = var.database_skip_final_snapshot
  final_snapshot_identifier = var.database_final_snapshot_identifier
  tags                      = local.tags
}

# Optional GPU host serving a real model with vLLM (issue #144); off by default.
module "model_host" {
  source                 = "./modules/model_host"
  count                  = var.gpu_model_enabled ? 1 : 0
  name                   = "gateflow-${var.environment}"
  region                 = var.region
  account_id             = var.aws_account_id
  vpc_id                 = module.network.vpc_id
  subnet_id              = module.network.private_subnet_ids[var.gpu_model_subnet_index]
  instance_type          = var.gpu_model_instance_type
  disk_gb                = var.gpu_model_disk_gb
  ami_id                 = var.gpu_model_ami_id
  served_model_name      = var.gpu_model_served_name
  max_num_seqs           = var.gpu_model_max_num_seqs
  max_model_len          = var.gpu_model_max_model_len
  extra_args             = var.gpu_model_extra_args
  api_key_ssm_parameter  = var.gpu_model_api_key_ssm_parameter
  gpu_memory_utilization = var.gpu_model_gpu_memory_utilization
  # The direct baselines of T0, T1 and T5 run on the load-generator node.
  client_security_group_ids = merge(
    { services = module.cluster.service_security_group_id },
    var.gpu_model_generator_access ? { "load-generator" = module.cluster.generator_security_group_id } : {}
  )
  tags = local.tags
}
