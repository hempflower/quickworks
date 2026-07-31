terraform {
  required_version = ">= 1.8.0"

  backend "local" {}

  required_providers {
    incus = {
      source  = "lxc/incus"
      version = "~> 1.1"
    }
  }
}

variable "workspace_id" {
  type = string
}

variable "transition" {
  type = string

  validation {
    condition     = contains(["start", "stop", "delete"], var.transition)
    error_message = "transition must be start, stop, or delete."
  }
}

variable "startup_script" {
  type      = string
  sensitive = true

  validation {
    condition     = startswith(trimspace(var.startup_script), "#!")
    error_message = "startup_script must start with a shebang."
  }
}

locals {
  running  = var.transition == "start"
  deleting = var.transition == "delete"

  image         = "images:ubuntu/24.04/cloud"
  instance_name = "qw-${var.workspace_id}"
}

resource "incus_instance" "workspace" {
  count = local.deleting ? 0 : 1

  name      = local.instance_name
  image     = local.image
  type      = "virtual-machine"
  running   = local.running
  ephemeral = false
  profiles  = ["default"]

  config = {
    "limits.cpu"          = "2"
    "limits.memory"       = "4GiB"
    "security.secureboot" = "false"
  }

  wait_for {
    type = "agent"
  }

  dynamic "file" {
    for_each = local.running ? [1] : []

    content {
      content     = var.startup_script
      target_path = "/usr/local/bin/quickworks-startup"
      uid         = 0
      gid         = 0
      mode        = "0700"
    }
  }

  exec = local.running ? {
    "00-startup" = {
      command = ["/usr/local/bin/quickworks-startup"]
      timeout = "10m"
      trigger = "once"
    }
  } : null

  # The bootstrap files are creation-time data. Do not remove them during a
  # stop: the provider first powers the VM off, then cannot access its files.
  lifecycle {
    ignore_changes = [file, exec]
  }
}

output "resource_id" {
  value = local.deleting ? null : incus_instance.workspace[0].name
}
