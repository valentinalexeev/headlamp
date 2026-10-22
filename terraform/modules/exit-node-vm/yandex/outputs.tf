output "instance_id" {
  value = yandex_compute_instance.this.id
}

output "internal_ip" {
  value = yandex_compute_instance.this.network_interface[0].ip_address
}

output "external_ip" {
  value = yandex_compute_instance.this.network_interface[0].nat_ip_address
}
