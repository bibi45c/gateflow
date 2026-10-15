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
