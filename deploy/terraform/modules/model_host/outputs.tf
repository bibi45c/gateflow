output "instance_id" { value = aws_instance.model.id }
output "private_ip" { value = aws_instance.model.private_ip }
output "base_url" { value = "http://${aws_instance.model.private_ip}:${var.port}" }
output "served_model_name" { value = var.served_model_name }
output "security_group_id" { value = aws_security_group.model.id }
output "role_arn" { value = aws_iam_role.model.arn }
output "ami_id" { value = aws_instance.model.ami }
