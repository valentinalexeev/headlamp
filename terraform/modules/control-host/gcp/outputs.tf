output "public_address" {
  description = "Point the DNS names of headscale, the enrolment listener and the funnel wildcards here (A records)."
  value       = google_compute_address.this.address
}

output "instance_name" {
  description = "For SSH through OS Login and IAP: gcloud compute ssh <name> --zone <zone> --tunnel-through-iap."
  value       = google_compute_instance.this.name
}

output "data_disk" {
  value = google_compute_disk.data.name
}

output "service_account_email" {
  description = "The host's service account. Grant it more roles if the host needs them."
  value       = google_service_account.this.email
}

output "network_tag" {
  description = "The network tag of the host, for more firewall rules."
  value       = local.tag
}
