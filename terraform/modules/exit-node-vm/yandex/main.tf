data "yandex_compute_image" "family" {
  count     = var.image_id == null ? 1 : 0
  family    = var.image_family
  folder_id = "standard-images"
}

resource "yandex_compute_instance" "this" {
  name        = var.name
  folder_id   = var.folder_id
  zone        = var.zone
  platform_id = var.platform_id

  resources {
    cores         = var.cores
    memory        = var.memory
    core_fraction = var.core_fraction
  }

  scheduling_policy {
    preemptible = var.preemptible
  }

  boot_disk {
    initialize_params {
      image_id = coalesce(var.image_id, try(data.yandex_compute_image.family[0].id, null))
      size     = var.boot_disk_size
      type     = var.boot_disk_type
    }
  }

  network_interface {
    subnet_id = var.subnet_id
    nat       = true
  }

  metadata = merge(
    {
      ssh-keys = "${var.ssh_user}:${var.ssh_public_key}"
    },
    var.user_data != "" ? { user-data = var.user_data } : {},
    var.extra_metadata
  )

  # A newer image in the family, or a new image_id, must not replace a running node. To redeploy on purpose, run
  #   terraform apply -replace='module.<name>.yandex_compute_instance.this'
  lifecycle {
    ignore_changes = [boot_disk[0].initialize_params[0].image_id]
  }
}
