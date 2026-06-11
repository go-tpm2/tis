# go-tpm2/tis

[![CI](https://github.com/go-tpm2/tis/actions/workflows/ci.yml/badge.svg)](https://github.com/go-tpm2/tis/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-tpm2/tis.svg)](https://pkg.go.dev/github.com/go-tpm2/tis)
[![Coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)](#conventions)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)

A pure-Go TPM **TIS / FIFO** MMIO transport for the
[`go-tpm2`](https://github.com/go-tpm2) stack. **v0.1.0.**

It drives the TCG PC Client TIS (FIFO) register handshake over the
platform-provided [`common.Regs`](https://github.com/go-tpm2/common)
MMIO accessor and satisfies
[`common.Transport`](https://github.com/go-tpm2/common), exchanging one
fully-marshaled TPM 2.0 command buffer for the full response buffer.

Sibling repos: [`common`](https://github.com/go-tpm2/common) (interfaces +
codec), [`crb`](https://github.com/go-tpm2/crb) (the CRB transport
alternative), [`tpm2`](https://github.com/go-tpm2/tpm2) (the command
layer that rides on this `Transport`), and
[`validate`](https://github.com/go-tpm2/validate) (live swtpm validation).

## Install

```sh
go get github.com/go-tpm2/tis
```

## Usage

```go
import (
	common "github.com/go-tpm2/common"
	"github.com/go-tpm2/tis"
)

// regs is your platform's MMIO window over the locality-0 register page
// (an mmap of /dev/mem at 0xFED4_0000, a hypervisor stub, a test fake…).
t, err := tis.Open(regs)
if err != nil {
	// no TPM present, or locality 0 could not be claimed
}

// *tis.TIS satisfies common.Transport — feed it to go-tpm2/tpm2:
//   tpm := tpm2.New(t); tpm.Startup(uint16(common.SUClear))

// …or send a raw command buffer:
rsp, err := t.Send(common.BuildCommand(uint16(common.TagNoSessions),
	uint32(common.CCGetRandom), []byte{0x00, 0x08}))
```

## Scope

`Open` validates presence via `TPM_DID_VID` and claims locality 0 via the
`TPM_ACCESS` handshake. `Send` runs the FIFO command sequence: move the
TPM to *Ready* (`STS.commandReady`), stream the command into
`TPM_DATA_FIFO` respecting `burstCount` and the `Expect` bit, start
execution (`STS.tpmGo`), poll `STS.dataAvail`, read the 10-byte header to
learn `responseSize`, drain the rest honoring `burstCount`, then release
the locality. Every busy-wait is bounded and returns a typed error.

## Authority

Register offsets, bit fields, and the command sequence follow the TCG
**PC Client Platform TPM Profile (PTP) Specification**, "FIFO Interface"
chapter (which folds in the legacy TCG PC Client Specific TPM Interface
Specification (TIS) 1.3). The TPM payloads are big-endian per TCG "TPM
2.0 Part 1"; the TIS control registers are native-endian MMIO via
`common.Regs`. Each register/bit is cited at its definition in
[`regs.go`](regs.go) and each handshake step in [`tis.go`](tis.go). The
two driver-side safety values (the spin budget and the response cap) are
marked `INFERRED:`.

## Conventions

Pure Go, `CGO_ENABLED=0`, no architecture-specific assembly, big-endian
TPM wire (via `common`), BSD-3-Clause on every file, `GOWORK=off`, 100%
statement coverage.

```sh
GOWORK=off go test -cover ./...
```

The two driver-side safety values are validated against a real `swtpm`
0.10.1 under QEMU `-device tpm-tis` by the
[`validate`](https://github.com/go-tpm2/validate) harness.

## Specifications

- TCG PC Client Platform TPM Profile (**PTP**) — *FIFO Interface* (folds in the
  legacy TCG PC Client TPM Interface Specification (TIS) 1.3).
- TCG TPM 2.0 Library, Parts 1–4 (wire format, via `common`).

## License

BSD-3-Clause. See [LICENSE](LICENSE).
