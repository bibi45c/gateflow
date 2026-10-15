mock_provider "aws" {}
variables {
  name                      = "gateflow-test"
  vpc_id                    = "vpc-test"
  private_subnet_ids        = ["subnet-a", "subnet-b"]
  service_security_group_id = "sg-services"
  tags                      = {}
}
run "private_database" {
  command = apply
  module { source = "./modules/database" }
  variables {
    deletion_protection = false
    skip_final_snapshot = true
  }
  assert {
    condition     = !aws_db_instance.main.publicly_accessible && !aws_db_instance.main.multi_az && aws_db_instance.main.instance_class == "db.t4g.small"
    error_message = "Use the budgeted private Single-AZ instance."
  }
  assert {
    condition     = aws_db_instance.main.storage_encrypted && aws_db_instance.main.allocated_storage == 20 && aws_db_instance.main.manage_master_user_password
    error_message = "Encrypt the database and keep its password out of Terraform."
  }
  assert {
    condition     = aws_vpc_security_group_ingress_rule.database.referenced_security_group_id == "sg-services" && aws_vpc_security_group_ingress_rule.database.from_port == 5432
    error_message = "Only service nodes may connect to PostgreSQL."
  }
}
run "snapshot_required" {
  command = plan
  module { source = "./modules/database" }
  variables { final_snapshot_identifier = "" }
  expect_failures = [var.final_snapshot_identifier]
}
run "safe_snapshot" {
  command = apply
  module { source = "./modules/database" }
  variables { final_snapshot_identifier = "gateflow-final-test" }
  assert {
    condition     = aws_db_instance.main.deletion_protection && !aws_db_instance.main.skip_final_snapshot
    error_message = "Default mode must protect deletion and preserve a snapshot."
  }
}
