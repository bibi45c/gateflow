module "network" {
  source             = "./modules/network"
  name               = "gateflow-${var.environment}"
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones
  tags               = local.tags
}

