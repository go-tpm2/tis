# go-tpm2/tis

A pure-Go TPM **TIS / FIFO** MMIO transport for the
[`go-tpm2`](https://github.com/go-tpm2) stack. It drives the TCG PC
Client TIS (FIFO) register handshake over the platform-provided
[`common.Regs`](https://github.com/go-tpm2/common) MMIO accessor and
satisfies [`common.Transport`](https://github.com/go-tpm2/common),
exchanging one fully-marshaled TPM 2.0 command buffer for the full
response buffer.

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

Pure Go, `CGO_ENABLED=0`, no architecture-specific assembly,
BSD-3-Clause on every file, `GOWORK=off`, 100% statement coverage.

```sh
GOWORK=off go test -cover ./...
```
