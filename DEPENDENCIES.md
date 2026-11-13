# Dependencies

The core package `github.com/brmzkw/qcowstream` uses the Go standard library
only: no CGO, no external process, no QEMU at runtime.

| Module | Version | License | Used by | Why |
|---|---|---|---|---|
| `github.com/digitalocean/go-nbd` | v0.4.0 | Apache-2.0 | `nbdsource`, `cmd/qcowstream` | Pure-Go NBD client: negotiation, structured replies, meta contexts, BLOCK_STATUS. |
| `github.com/google/go-cmp` | v0.7.0 | BSD-3-Clause | indirect, through go-nbd | |
| `github.com/testcontainers/testcontainers-go` | v0.45.0 | MIT | `integration/` module only | Runs QEMU 8.2 and 10.0 containers for the integration tests. |

Apache-2.0 is compatible with ISC, MIT, BSD, Apache-2.0 and GPLv3
publication, not with GPLv2-only. The project's own license is the owner's
choice and is not set here.

QEMU (`qemu-img`, `qemu-io`, `qemu-nbd`) is a test oracle, installed in the
integration containers, never a runtime dependency.
