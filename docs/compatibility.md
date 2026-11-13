# Compatibility

## QEMU versions tested

| QEMU | Where | What |
|---|---|---|
| 8.2.2 (`ubuntu:24.04`, `qemu-utils 1:8.2.2+ds-0ubuntu1.18`) | integration tests, CI | All suites: full, chain, zero flags, sparse sources, `qemu-nbd -B`, 100 TiB increment |
| 10.0.13 (`debian:trixie`, `qemu-utils 1:10.0.13+ds-0+deb13u1`) | integration tests, CI | All suites |
| 10.2.2 (Homebrew, macOS) | manual | `qemu-img check` and `compare` of full images; `qemu-nbd` throughput (docs/measurements.md) |

The integration tests print the exact `qemu-img --version` of each container.
Packages move; the versions above are those of 2026-11-13.

## Not tested

- **Proxmox VE's QEMU** (pve-qemu 11.1.3 at the time of writing): not run. Its
  backup-access export was studied from source only (see `proxmox.md`).
- **Other QCOW2 readers**, such as libvirt tooling built on its own parser.

## Go

Go 1.26. The core package needs only the standard library.
