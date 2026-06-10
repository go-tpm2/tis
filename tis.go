// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tis authors. All rights reserved.

// Package tis implements a pure-Go TPM TIS / FIFO MMIO transport. It
// drives the TCG PC Client TIS (FIFO) register handshake over a
// platform-provided common.Regs MMIO accessor and satisfies
// common.Transport, exchanging one fully-marshaled TPM 2.0 command
// buffer for the full response buffer.
//
// Authority for the register handshake is the TCG "PC Client Platform
// TPM Profile (PTP) Specification", "FIFO Interface" chapter, which
// supersedes (and folds in) the legacy TCG "PC Client Specific TPM
// Interface Specification (TIS)" 1.3. Citations below name the relevant
// register/clause.
//
// Conventions: pure Go, CGO_ENABLED=0, no architecture-specific assembly,
// BSD-3-Clause on every file, GOWORK=off, 100% statement coverage.
package tis

import (
	common "github.com/go-tpm2/common"
)

// Error is the typed, constant error returned by this package, mirroring
// common.Error so callers may compare values with ==.
type Error string

// Error implements the error interface.
func (e Error) Error() string { return string(e) }

// Error sentinels.
const (
	// ErrNoLocality is returned when locality 0 cannot be claimed: the
	// TPM never asserts ACCESS.activeLocality after a requestUse.
	// PTP "TPM_ACCESS_x", activeLocality.
	ErrNoLocality = Error("tis: timed out claiming locality 0")
	// ErrNotPresent is returned when the interface does not look like a
	// TPM (TPM_DID_VID reads back all-ones / all-zeros). PTP
	// "TPM_DID_VID_x".
	ErrNotPresent = Error("tis: no TPM present at register window")
	// ErrCommandReady is returned when the TPM never asserts
	// STS.commandReady, so a command cannot be staged. PTP "TPM_STS_x",
	// commandReady.
	ErrCommandReady = Error("tis: timed out waiting for commandReady")
	// ErrBurstTimeout is returned when burstCount stays zero for the
	// whole bound, so no FIFO bytes can be moved. PTP "TPM_STS_x",
	// burstCount.
	ErrBurstTimeout = Error("tis: timed out waiting for non-zero burstCount")
	// ErrExpect is returned when the TPM's Expect bit disagrees with the
	// driver's view of the command transfer: it clears before the last
	// byte (TPM thinks the command is complete early) or stays set after
	// the last byte (TPM still wants more). PTP "TPM_STS_x", Expect.
	ErrExpect = Error("tis: Expect/transfer length mismatch")
	// ErrDataAvail is returned when the TPM never asserts STS.dataAvail
	// after tpmGo, so no response can be read. PTP "TPM_STS_x",
	// dataAvail.
	ErrDataAvail = Error("tis: timed out waiting for response dataAvail")
	// ErrShortResponse is returned when the FIFO yields fewer than the
	// 10-byte TPM 2.0 header. TCG "TPM 2.0 Part 1", response header.
	ErrShortResponse = Error("tis: response shorter than TPM header")
	// ErrResponseSize is returned when the header's responseSize is
	// implausible (smaller than the header or larger than the maximum
	// the driver will buffer). TCG "TPM 2.0 Part 1", responseSize.
	ErrResponseSize = Error("tis: response size out of range")
)

// maxSpins bounds every busy-wait loop. The TIS handshake has no
// hardware timer here, so progress is bounded by a spin budget rather
// than wall-clock time; the platform's Regs accessor is expected to make
// each access observe real device state. PTP leaves the polling cadence
// to the implementation.
//
// INFERRED: the spin budget itself is not a spec value; the PTP only
// requires bounded polling with implementation-defined timeouts.
const maxSpins = 1 << 20

// maxResponse caps the response buffer the driver will assemble. The TPM
// 2.0 architecture allows responses up to the TPM's maximum response
// size; 4096 bytes covers every PC Client command/response and guards a
// malformed responseSize from forcing an unbounded allocation.
//
// INFERRED: 4096 is a driver-side safety cap, not a fixed PTP constant
// (the PTP bounds buffers by the negotiated MAX_RESPONSE_SIZE).
const maxResponse = 4096

// TIS is a TPM TIS / FIFO transport bound to a single locality-0 register
// window. It satisfies common.Transport.
type TIS struct {
	r common.Regs
}

// writeFIFO streams p into the TPM_DATA_FIFO_x register. Unlike the CRB
// command buffer, the TIS data FIFO is a single fixed-address register:
// every byte is written to the same offset (regDataFIFO), and the TPM
// advances its internal pointer. PTP "TPM_DATA_FIFO_x". This is why the
// driver cannot use common.WriteBytes (which increments the offset for
// the sequential CRB buffer).
func writeFIFO(r common.Regs, p []byte) {
	for _, b := range p {
		r.Write8(regDataFIFO, b)
	}
}

// readFIFOBytes drains len(p) bytes from the fixed-address
// TPM_DATA_FIFO_x register. PTP "TPM_DATA_FIFO_x"; counterpart of
// writeFIFO.
func readFIFOBytes(r common.Regs, p []byte) {
	for i := range p {
		p[i] = r.Read8(regDataFIFO)
	}
}

// Open binds a TIS transport to the locality-0 register window presented
// by r. It validates that a TPM is present (TPM_DID_VID is neither
// all-zeros nor all-ones) and claims locality 0 via the ACCESS handshake
// so the first Send can proceed. PTP "TPM_DID_VID_x" and "TPM_ACCESS_x".
func Open(r common.Regs) (*TIS, error) {
	t := &TIS{r: r}

	// Presence check. A populated TPM returns a real vendor/device id;
	// an unmapped or absent interface floats to 0x00000000 or
	// 0xFFFFFFFF. PTP "TPM_DID_VID_x".
	didvid := r.Read32(regDIDVID)
	if didvid == 0x00000000 || didvid == 0xFFFFFFFF {
		return nil, ErrNotPresent
	}

	// Claim locality 0 up front so it is ready for the first command.
	if err := t.requestLocality(); err != nil {
		return nil, err
	}
	return t, nil
}

// requestLocality claims locality 0. It writes ACCESS.requestUse and
// waits for ACCESS.activeLocality to assert (honoring tpmRegValidSts
// before trusting the bits). PTP "TPM_ACCESS_x": software requests a
// locality by writing requestUse=1 and observes ownership through
// activeLocality. If activeLocality is already set the locality is
// already ours and the request is a no-op.
func (t *TIS) requestLocality() error {
	a := t.r.Read8(regAccess)
	if a&accessTPMRegValidSts != 0 && a&accessActiveLocality != 0 {
		return nil
	}

	// PTP "TPM_ACCESS_x", requestUse: write 1 to request the locality.
	t.r.Write8(regAccess, accessRequestUse)

	for i := 0; i < maxSpins; i++ {
		a = t.r.Read8(regAccess)
		// Trust activeLocality only once tpmRegValidSts says the
		// register contents are valid. PTP "TPM_ACCESS_x",
		// tpmRegValidSts.
		if a&accessTPMRegValidSts != 0 && a&accessActiveLocality != 0 {
			return nil
		}
	}
	return ErrNoLocality
}

// releaseLocality relinquishes locality 0 by writing
// ACCESS.activeLocality=1. PTP "TPM_ACCESS_x", activeLocality: writing 1
// to activeLocality releases the active locality so another may claim it.
func (t *TIS) releaseLocality() {
	t.r.Write8(regAccess, accessActiveLocality)
}

// readSts returns a TPM_STS_x value whose stsValid bit is set, so that the
// stsValid-qualified status bits (Expect and dataAvail) can be trusted. PTP
// "TPM_STS_x", stsValid: stsValid qualifies Expect and dataAvail ONLY. It
// does NOT qualify commandReady or burstCount, which are valid independently
// of stsValid — see readStsRaw and the note in waitCommandReady/waitBurst.
//
// CONFIRMED on a live swtpm 0.10.1 under QEMU (-device tpm-tis): in the Ready
// state STS reads 0x04100040 — commandReady=1, burstCount=4096, stsValid=0 —
// persistently. A previous version of this driver gated commandReady and
// burstCount behind stsValid and hung forever in that state. stsValid is
// asserted by the TPM only once Expect/dataAvail are meaningful.
func (t *TIS) readSts() (uint32, bool) {
	for i := 0; i < maxSpins; i++ {
		s := t.r.Read32(regSts)
		if s&stsValid != 0 {
			return s, true
		}
	}
	return 0, false
}

// Send transmits one fully-marshaled TPM 2.0 command buffer and returns
// the full response buffer (including the 10-byte header), satisfying
// common.Transport.
//
// The flow follows the PTP "FIFO Interface" command sequence:
//
//  1. Ensure locality 0 is active (ACCESS.requestUse / activeLocality).
//  2. Move the TPM to the Ready state: write STS.commandReady=1 and wait
//     until STS.commandReady reads back set. PTP "TPM_STS_x",
//     commandReady; PTP FIFO state diagram "Idle -> Ready".
//  3. Write the command to DATA_FIFO honoring burstCount (STS[8:23]):
//     never write more bytes than the TPM currently advertises, and
//     check STS.Expect after each chunk. Expect must stay set while more
//     bytes remain and must clear on the last byte. PTP "TPM_STS_x",
//     burstCount and Expect; PTP FIFO state diagram "Reception".
//  4. Start execution: write STS.tpmGo=1. PTP "TPM_STS_x", tpmGo.
//  5. Poll STS.dataAvail, then read the response from DATA_FIFO honoring
//     burstCount: first the 10-byte header to learn responseSize, then
//     the remaining bytes. PTP "TPM_STS_x", dataAvail and burstCount;
//     PTP FIFO state diagram "Completion".
//  6. Release locality 0 (ACCESS.activeLocality=1). PTP "TPM_ACCESS_x".
func (t *TIS) Send(cmd []byte) ([]byte, error) {
	// 1. Locality.
	if err := t.requestLocality(); err != nil {
		return nil, err
	}
	defer t.releaseLocality()

	// 2. Move to Ready. PTP "TPM_STS_x", commandReady: writing 1 aborts
	// any in-progress command and arms the FIFO for a fresh one.
	t.r.Write32(regSts, stsCommandReady)
	if err := t.waitCommandReady(); err != nil {
		return nil, err
	}

	// 3. Write command honoring burstCount and Expect.
	if err := t.writeCommand(cmd); err != nil {
		return nil, err
	}

	// 4. Go. PTP "TPM_STS_x", tpmGo.
	t.r.Write32(regSts, stsTPMGo)

	// 5. Read response.
	rsp, err := t.readResponse()
	if err != nil {
		return nil, err
	}

	// 6. Release happens via the deferred releaseLocality.
	return rsp, nil
}

// waitCommandReady waits until STS.commandReady is observed set. PTP
// "TPM_STS_x", commandReady: commandReady is NOT one of the bits qualified by
// stsValid, so it must be read raw — a live swtpm holds commandReady=1 with
// stsValid=0 in the Ready state, so gating this on stsValid hangs forever
// (CONFIRMED on swtpm 0.10.1 / QEMU tpm-tis; see readSts).
func (t *TIS) waitCommandReady() error {
	for i := 0; i < maxSpins; i++ {
		s := t.r.Read32(regSts)
		if s&stsCommandReady != 0 {
			return nil
		}
	}
	return ErrCommandReady
}

// writeCommand streams cmd into DATA_FIFO. It writes at most burstCount
// bytes per pass (PTP "TPM_STS_x", burstCount), waiting for a non-zero
// burstCount when the TPM throttles to zero, and validates STS.Expect
// after every chunk: Expect must remain set while bytes remain and must
// clear once the final byte has been accepted. PTP "TPM_STS_x", Expect;
// PTP FIFO "Reception".
func (t *TIS) writeCommand(cmd []byte) error {
	off := 0
	for off < len(cmd) {
		burst, err := t.waitBurst()
		if err != nil {
			return err
		}

		// Never write past the end of the command or past the burst the
		// TPM advertises. PTP "TPM_STS_x", burstCount.
		n := burst
		if remaining := len(cmd) - off; n > remaining {
			n = remaining
		}
		writeFIFO(t.r, cmd[off:off+n])
		off += n

		// After the chunk, read status to inspect Expect. PTP
		// "TPM_STS_x", Expect.
		s, ok := t.readSts()
		if !ok {
			return ErrExpect
		}
		expect := s&stsExpect != 0
		if off < len(cmd) {
			// More bytes remain: the TPM must still expect data.
			if !expect {
				return ErrExpect
			}
		} else {
			// All bytes written: the TPM must no longer expect data.
			if expect {
				return ErrExpect
			}
		}
	}
	return nil
}

// waitBurst returns a non-zero burstCount, polling while the TPM
// advertises zero (its FIFO is momentarily full). PTP "TPM_STS_x",
// burstCount: a zero burstCount means software must wait before moving more
// bytes. burstCount is NOT qualified by stsValid, so STS is read raw — a live
// swtpm advertises burstCount=4096 with stsValid=0 in the Ready state
// (CONFIRMED on swtpm 0.10.1 / QEMU tpm-tis; see readSts).
func (t *TIS) waitBurst() (int, error) {
	for i := 0; i < maxSpins; i++ {
		s := t.r.Read32(regSts)
		if b := burstCount(s); b > 0 {
			return b, nil
		}
	}
	return 0, ErrBurstTimeout
}

// readResponse waits for STS.dataAvail, then drains DATA_FIFO honoring
// burstCount. It reads the 10-byte header first to recover responseSize,
// then the remainder. PTP "TPM_STS_x", dataAvail and burstCount; TCG
// "TPM 2.0 Part 1", response header.
func (t *TIS) readResponse() ([]byte, error) {
	if err := t.waitDataAvail(); err != nil {
		return nil, err
	}

	// Read the fixed 10-byte header to learn responseSize.
	header := make([]byte, common.HeaderSize)
	if err := t.readFIFO(header); err != nil {
		return nil, err
	}

	size, _ := common.GetU32(header, 2) // responseSize at offset 2.
	if int(size) < common.HeaderSize || int(size) > maxResponse {
		return nil, ErrResponseSize
	}

	rsp := make([]byte, size)
	copy(rsp, header)
	if int(size) > common.HeaderSize {
		if err := t.readFIFO(rsp[common.HeaderSize:]); err != nil {
			return nil, err
		}
	}
	return rsp, nil
}

// waitDataAvail waits until STS.dataAvail is observed set (stsValid
// honored). PTP "TPM_STS_x", dataAvail.
func (t *TIS) waitDataAvail() error {
	for i := 0; i < maxSpins; i++ {
		s, ok := t.readSts()
		if !ok {
			return ErrDataAvail
		}
		if s&stsDataAvail != 0 {
			return nil
		}
	}
	return ErrDataAvail
}

// readFIFO fills p from DATA_FIFO, honoring burstCount and re-checking
// dataAvail so it never reads past the available data. PTP "TPM_STS_x",
// burstCount and dataAvail.
func (t *TIS) readFIFO(p []byte) error {
	off := 0
	for off < len(p) {
		burst, err := t.waitBurst()
		if err != nil {
			return err
		}

		n := burst
		if remaining := len(p) - off; n > remaining {
			n = remaining
		}
		readFIFOBytes(t.r, p[off:off+n])
		off += n

		// While more bytes are still expected, dataAvail must remain
		// set; if the TPM drops it early the response is short. PTP
		// "TPM_STS_x", dataAvail.
		if off < len(p) {
			s, ok := t.readSts()
			if !ok {
				return ErrShortResponse
			}
			if s&stsDataAvail == 0 {
				return ErrShortResponse
			}
		}
	}
	return nil
}
