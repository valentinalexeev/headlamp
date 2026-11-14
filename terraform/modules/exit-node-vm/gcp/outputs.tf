output "instance_name" {
  value       = google_compute_instance.this.name
  description = "Instance name: the node name that node-control looks up."
}

output "zone" {
  value       = google_compute_instance.this.zone
  description = "Zone of the instance (for GCP_ZONES, or the node block of node-control)."
}

output "service_account_email" {
  value       = google_service_account.this.email
  description = "The node's service account."
}

output "public_ip" {
  value       = google_compute_instance.this.network_interface[0].access_config[0].nat_ip
  description = "Ephemeral external IPv4 address. It can change on a stop and start; the tailnet identity does not."
}
