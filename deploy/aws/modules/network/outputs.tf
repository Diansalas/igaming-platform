output "vpc_id" {
  value = aws_vpc.this.id
}

output "vpc_cidr" {
  value = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  value = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  value = aws_subnet.private[*].id
}

output "private_subnet_cidrs" {
  value = aws_subnet.private[*].cidr_block
}

output "availability_zones" {
  value = local.azs
}

output "nat_gateway_id" {
  description = "Null when enable_nat_gateway = false."
  value       = one(aws_nat_gateway.this[*].id)
}
