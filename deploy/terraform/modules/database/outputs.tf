output "database_address" { value = aws_db_instance.main.address }
output "database_port" { value = aws_db_instance.main.port }
output "database_name" { value = aws_db_instance.main.db_name }
output "master_secret_arn" { value = try(aws_db_instance.main.master_user_secret[0].secret_arn, null) }
