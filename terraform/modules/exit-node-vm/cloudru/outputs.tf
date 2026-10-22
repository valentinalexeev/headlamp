output "instance_id" {
  value = cloudru_evolution_compute_vm.this.id
}

output "external_ip" {
  value = cloudru_evolution_compute_external_ip.this.ip_address
}
