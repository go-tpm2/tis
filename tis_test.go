// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tis authors. All rights reserved.

package tis

import (
	"bytes"
	"testing"

	common "github.com/go-tpm2/common"
)

// fakeTPM is a software model of the TIS / FIFO register state machine.
// It simulates ACCESS/STS bit transitions, an advertised burstCount, and
// a DATA_FIFO that accepts the command and yields a canned response. The
// knobs let each test drive a specific branch of Send.
type fakeTPM struct {
	access uint32 // current TPM_ACCESS_x value (low byte meaningful)
	sts    uint32 // current TPM_STS_x value (status bits + burstCount)
	didvid uint32 // TPM_DID_VID_x reported value

	// burst is the burstCount the model advertises in STS[8:23]. When
	// burstZeroFor > 0 it first reports 0 that many polls (throttle),
	// then reports burst.
	burst        int
	burstZeroFor int

	// command capture and canned response.
	cmdBuf  []byte
	expect  int    // bytes still expected before Expect clears
	resp    []byte // canned response bytes to hand back via the FIFO
	respPos int    // read cursor into resp

	// behavioral knobs for the failure branches.
	denyLocality   bool // never assert activeLocality
	localityAfter  int  // assert activeLocality only after this many ACCESS reads
	accessReads    int  // ACCESS read counter (drives localityAfter)
	noCommandReady bool // never assert commandReady
	noDataAvail    bool // never assert dataAvail
	noValidSts     bool // never assert stsValid
	expectEarly    bool // clear Expect one byte too soon
	expectLate     bool // keep Expect set after the last byte
	dropDataAvail  bool // drop dataAvail mid-response read

	// validOnlyWithExpectOrData models a real swtpm/QEMU TPM: stsValid (the
	// bit that qualifies Expect and dataAvail per PTP) is asserted ONLY when
	// Expect or dataAvail is meaningful — NOT in the bare Ready state. On real
	// hardware the Ready state reports commandReady=1, burstCount=N, but
	// stsValid=0 (CONFIRMED on swtpm 0.10.1 via the validate harness). With
	// this set the driver must read commandReady/burstCount raw and still
	// complete a Send.
	validOnlyWithExpectOrData bool

	commandReady bool // model is in Ready state
	went         bool // tpmGo was written
}

// newFakeTPM builds a model primed for a happy-path exchange: present
// TPM, a canned response, and a steady burstCount.
func newFakeTPM(resp []byte) *fakeTPM {
	return &fakeTPM{
		didvid: 0x0000_1014, // plausible vendor/device id (non 0/-1)
		burst:  64,
		resp:   resp,
	}
}

// recomputeSts folds the model's logical state into the STS register
// value, including the burstCount field, mirroring how real hardware
// reflects its FIFO state.
func (f *fakeTPM) recomputeSts() {
	var s uint32
	if f.commandReady && !f.noCommandReady {
		s |= stsCommandReady
	}
	dataAvail := f.went && !f.noDataAvail && f.respPos < len(f.resp) && !f.dropDataAvail
	if dataAvail {
		s |= stsDataAvail
	}
	expect := (f.expect > 0 && !f.expectEarly) || f.expectLate
	if expect {
		s |= stsExpect
	}
	// stsValid qualifies Expect and dataAvail. By default the model asserts it
	// whenever the register is stable. validOnlyWithExpectOrData models real
	// hardware: stsValid is 0 only in the bare Ready state (no command bytes
	// staged yet, no response pending) and asserts once the Reception phase
	// has begun (any FIFO byte written, so Expect is meaningful) or a response
	// is available — matching swtpm, where the Ready state reports
	// commandReady=1/burst=N/stsValid=0 but the last command byte and the
	// response both report a valid Expect/dataAvail.
	if !f.noValidSts {
		receptionOrCompletion := len(f.cmdBuf) > 0 || f.went
		if !f.validOnlyWithExpectOrData || receptionOrCompletion {
			s |= stsValid
		}
	}
	// burstCount, with optional throttle to zero for a few polls.
	b := f.burst
	if f.burstZeroFor > 0 {
		f.burstZeroFor--
		b = 0
	}
	s |= uint32(b&stsBurstMask) << stsBurstShift
	// Preserve nothing else; tpmGo/responseRetry are write-1 triggers.
	f.sts = s
}

func (f *fakeTPM) Read8(off uint32) uint8 {
	switch off {
	case regAccess:
		f.accessReads++
		a := accessTPMRegValidSts // register bits always valid here
		switch {
		case f.denyLocality:
			// never active
		case f.localityAfter > 0:
			// active only once enough ACCESS reads have happened,
			// forcing requestLocality to spin through its loop.
			if f.accessReads > f.localityAfter {
				a |= accessActiveLocality
			}
		default:
			a |= accessActiveLocality
		}
		return a
	case regDataFIFO:
		if f.respPos < len(f.resp) {
			b := f.resp[f.respPos]
			f.respPos++
			return b
		}
		return 0
	}
	return 0
}

func (f *fakeTPM) Read32(off uint32) uint32 {
	switch off {
	case regSts:
		f.recomputeSts()
		return f.sts
	case regDIDVID:
		return f.didvid
	}
	return 0
}

func (f *fakeTPM) Write8(off uint32, v uint8) {
	switch off {
	case regAccess:
		// requestUse / activeLocality writes are observed but the
		// Read8 path models ownership directly.
		_ = v
	case regDataFIFO:
		f.cmdBuf = append(f.cmdBuf, v)
		if f.expect > 0 {
			f.expect--
		}
	}
}

func (f *fakeTPM) Write32(off uint32, v uint32) {
	if off != regSts {
		return
	}
	switch {
	case v&stsCommandReady != 0:
		f.commandReady = true
	case v&stsTPMGo != 0:
		f.went = true
	}
}

// armExpect tells the model how many command bytes to expect, so the
// Expect bit clears exactly on the last byte.
func (f *fakeTPM) armExpect(n int) { f.expect = n }

func TestOpenHappyPath(t *testing.T) {
	f := newFakeTPM(nil)
	tp, err := Open(f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if tp == nil {
		t.Fatal("Open returned nil TIS")
	}
}

func TestOpenNotPresentZero(t *testing.T) {
	f := newFakeTPM(nil)
	f.didvid = 0x0000_0000
	if _, err := Open(f); err != ErrNotPresent {
		t.Fatalf("want ErrNotPresent, got %v", err)
	}
}

func TestOpenNotPresentAllOnes(t *testing.T) {
	f := newFakeTPM(nil)
	f.didvid = 0xFFFF_FFFF
	if _, err := Open(f); err != ErrNotPresent {
		t.Fatalf("want ErrNotPresent, got %v", err)
	}
}

func TestOpenNoLocality(t *testing.T) {
	f := newFakeTPM(nil)
	f.denyLocality = true
	if _, err := Open(f); err != ErrNoLocality {
		t.Fatalf("want ErrNoLocality, got %v", err)
	}
}

// goodResponse builds a well-formed TPM 2.0 response of a given total
// size (>= header).
func goodResponse(t *testing.T, size int) []byte {
	t.Helper()
	if size < common.HeaderSize {
		t.Fatalf("bad test response size %d", size)
	}
	body := make([]byte, size-common.HeaderSize)
	for i := range body {
		body[i] = byte(i + 1)
	}
	// tag TagNoSessions, responseSize=size, rc=Success.
	r := common.PutU16(nil, uint16(common.TagNoSessions))
	r = common.PutU32(r, uint32(size))
	r = common.PutU32(r, uint32(common.RCSuccess))
	r = append(r, body...)
	return r
}

// sendOnce wires a model for a Send: arms Expect to the command length
// and pre-loads the canned response.
func runSend(t *testing.T, f *fakeTPM, cmd []byte) ([]byte, error) {
	t.Helper()
	tp, err := Open(f)
	if err != nil {
		return nil, err
	}
	f.armExpect(len(cmd))
	return tp.Send(cmd)
}

func TestSendHappyPath(t *testing.T) {
	resp := goodResponse(t, 32)
	f := newFakeTPM(resp)
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x00, 0x08})

	got, err := runSend(t, f, cmd)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Equal(got, resp) {
		t.Fatalf("response mismatch:\n got %x\nwant %x", got, resp)
	}
	if !bytes.Equal(f.cmdBuf, cmd) {
		t.Fatalf("command not delivered to FIFO:\n got %x\nwant %x", f.cmdBuf, cmd)
	}
}

func TestSendHeaderOnlyResponse(t *testing.T) {
	// responseSize == HeaderSize: exercises the no-body branch.
	resp := goodResponse(t, common.HeaderSize)
	f := newFakeTPM(resp)
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCShutdown), []byte{0x00, 0x00})
	got, err := runSend(t, f, cmd)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Equal(got, resp) {
		t.Fatalf("response mismatch:\n got %x\nwant %x", got, resp)
	}
}

func TestSendBurstThrottle(t *testing.T) {
	// burstCount reports 0 for several polls (full FIFO), then a small
	// non-zero burst, forcing the chunked write/read paths.
	resp := goodResponse(t, 48)
	f := newFakeTPM(resp)
	f.burst = 4        // small burst -> many chunks
	f.burstZeroFor = 3 // throttle to zero first
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), make([]byte, 30))
	got, err := runSend(t, f, cmd)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Equal(got, resp) {
		t.Fatalf("response mismatch")
	}
}

func TestSendNoLocalityOnSend(t *testing.T) {
	// Locality is claimable at Open but then denied at Send time.
	f := newFakeTPM(goodResponse(t, 32))
	tp, err := Open(f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.denyLocality = true
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), nil)
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrNoLocality {
		t.Fatalf("want ErrNoLocality, got %v", err)
	}
}

func TestSendCommandReadyTimeout(t *testing.T) {
	f := newFakeTPM(goodResponse(t, 32))
	f.noCommandReady = true
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), nil)
	if _, err := runSend(t, f, cmd); err != ErrCommandReady {
		t.Fatalf("want ErrCommandReady, got %v", err)
	}
}

func TestSendReadyStsValidClear(t *testing.T) {
	// Real-hardware semantics: in the Ready state the TPM reports
	// commandReady=1 and a non-zero burstCount but stsValid=0 (stsValid
	// qualifies only Expect/dataAvail). The driver must read commandReady and
	// burstCount raw and still drive the command to completion. This is the
	// exact behavior the validate harness CONFIRMED on a live swtpm 0.10.1,
	// where gating commandReady/burstCount behind stsValid hung forever.
	resp := goodResponse(t, 32)
	f := newFakeTPM(resp)
	f.validOnlyWithExpectOrData = true
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x00, 0x08})
	got, err := runSend(t, f, cmd)
	if err != nil {
		t.Fatalf("Send with stsValid clear in Ready state: %v", err)
	}
	if !bytes.Equal(got, resp) {
		t.Fatalf("response mismatch:\n got %x\nwant %x", got, resp)
	}
	if !bytes.Equal(f.cmdBuf, cmd) {
		t.Fatalf("command not delivered to FIFO:\n got %x\nwant %x", f.cmdBuf, cmd)
	}
}

func TestSendBurstTimeout(t *testing.T) {
	// burstCount stuck at zero for the whole bound during write.
	f := newFakeTPM(goodResponse(t, 32))
	f.burst = 0
	f.burstZeroFor = maxSpins + 10 // always zero
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	if _, err := runSend(t, f, cmd); err != ErrBurstTimeout {
		t.Fatalf("want ErrBurstTimeout, got %v", err)
	}
}

func TestSendExpectClearsEarly(t *testing.T) {
	// Expect clears before the last byte -> mismatch while bytes remain.
	f := newFakeTPM(goodResponse(t, 32))
	f.burst = 4
	f.expectEarly = true
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), make([]byte, 20))
	if _, err := runSend(t, f, cmd); err != ErrExpect {
		t.Fatalf("want ErrExpect, got %v", err)
	}
}

func TestSendExpectStaysSet(t *testing.T) {
	// Expect stays set after the final byte -> TPM still wants more.
	f := newFakeTPM(goodResponse(t, 32))
	f.expectLate = true
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01, 0x02})
	if _, err := runSend(t, f, cmd); err != ErrExpect {
		t.Fatalf("want ErrExpect, got %v", err)
	}
}

func TestSendExpectNoValidStsAfterChunk(t *testing.T) {
	// stsValid drops after the chunk write -> readSts fails in
	// writeCommand's Expect check.
	f := newFakeTPM(goodResponse(t, 32))
	f.burst = 64
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01, 0x02})
	// Flip noValidSts on once the whole command has reached the FIFO.
	w := &flipAfterReady{fakeTPM: f, cmdLen: len(cmd)}
	tp, err := Open(w)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrExpect {
		t.Fatalf("want ErrExpect, got %v", err)
	}
}

func TestSendDataAvailTimeout(t *testing.T) {
	f := newFakeTPM(goodResponse(t, 32))
	f.noDataAvail = true
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	if _, err := runSend(t, f, cmd); err != ErrDataAvail {
		t.Fatalf("want ErrDataAvail, got %v", err)
	}
}

func TestSendDataAvailNoValidSts(t *testing.T) {
	// stsValid drops right before the response phase -> readSts fails in
	// waitDataAvail. Use the flip wrapper to drop validity after tpmGo.
	f := newFakeTPM(goodResponse(t, 32))
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	w := &dropValidAfterGo{fakeTPM: f}
	tp, err := Open(w)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrDataAvail {
		t.Fatalf("want ErrDataAvail, got %v", err)
	}
}

func TestSendResponseSizeTooSmall(t *testing.T) {
	// Header claims a responseSize smaller than the header.
	resp := make([]byte, common.HeaderSize)
	resp = common.PutU16(resp[:0], uint16(common.TagNoSessions))
	resp = common.PutU32(resp, 4) // responseSize = 4 (< HeaderSize)
	resp = common.PutU32(resp, 0)
	f := newFakeTPM(resp)
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	if _, err := runSend(t, f, cmd); err != ErrResponseSize {
		t.Fatalf("want ErrResponseSize, got %v", err)
	}
}

func TestSendResponseSizeTooLarge(t *testing.T) {
	// Header claims a responseSize past maxResponse.
	resp := common.PutU16(nil, uint16(common.TagNoSessions))
	resp = common.PutU32(resp, maxResponse+1)
	resp = common.PutU32(resp, 0)
	f := newFakeTPM(resp)
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	if _, err := runSend(t, f, cmd); err != ErrResponseSize {
		t.Fatalf("want ErrResponseSize, got %v", err)
	}
}

func TestSendShortHeaderRead(t *testing.T) {
	// dataAvail asserts, but the FIFO holds fewer than 10 bytes: the
	// model drops dataAvail mid-header, so the header read sees a short
	// response. burst small enough to chunk the header.
	resp := []byte{0x80, 0x01, 0x00, 0x00} // only 4 bytes available
	f := newFakeTPM(resp)
	f.burst = 2
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	w := &dropDataMidRead{fakeTPM: f, dropAfter: 2}
	tp, err := Open(w)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrShortResponse {
		t.Fatalf("want ErrShortResponse, got %v", err)
	}
}

func TestSendShortBodyRead(t *testing.T) {
	// Header is fine and declares a body, but dataAvail drops during the
	// body read -> ErrShortResponse from readFIFO's body pass.
	resp := goodResponse(t, 40)
	f := newFakeTPM(resp)
	f.burst = 4
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	w := &dropDataMidRead{fakeTPM: f, dropAfter: common.HeaderSize + 4}
	tp, err := Open(w)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrShortResponse {
		t.Fatalf("want ErrShortResponse, got %v", err)
	}
}

// --- wrapper Regs that inject mid-flow state changes ---

// flipAfterReady drops stsValid once all command bytes have reached the
// FIFO, so the post-chunk Expect readSts in writeCommand fails (ok ==
// false), hitting that error branch.
type flipAfterReady struct {
	*fakeTPM
	cmdLen int
}

func (w *flipAfterReady) Read32(off uint32) uint32 {
	if off == regSts && len(w.fakeTPM.cmdBuf) >= w.cmdLen && w.cmdLen > 0 {
		w.fakeTPM.noValidSts = true
	}
	return w.fakeTPM.Read32(off)
}

// dropValidAfterGo drops stsValid once tpmGo has been written, to hit
// waitDataAvail's readSts-failure branch.
type dropValidAfterGo struct {
	*fakeTPM
}

func (w *dropValidAfterGo) Read32(off uint32) uint32 {
	if off == regSts && w.fakeTPM.went {
		w.fakeTPM.noValidSts = true
	}
	return w.fakeTPM.Read32(off)
}

// dropDataMidRead drops dataAvail after dropAfter FIFO bytes have been
// read, simulating a truncated response.
type dropDataMidRead struct {
	*fakeTPM
	dropAfter int
}

func (w *dropDataMidRead) Read32(off uint32) uint32 {
	if off == regSts && w.fakeTPM.respPos >= w.dropAfter {
		w.fakeTPM.dropDataAvail = true
	}
	return w.fakeTPM.Read32(off)
}

func TestOpenLocalityAfterSpin(t *testing.T) {
	// activeLocality is not set on the first ACCESS read (top-of-function
	// fast path misses), then asserts inside the requestLocality spin
	// loop, exercising the loop-success branch.
	f := newFakeTPM(nil)
	f.localityAfter = 1
	tp, err := Open(f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if tp == nil {
		t.Fatal("nil TIS")
	}
}

func TestSendBurstStuckDuringRead(t *testing.T) {
	// burstCount drops to zero permanently once the response read begins,
	// so readFIFO's waitBurst times out and readFIFO propagates the
	// error.
	f := newFakeTPM(goodResponse(t, 40))
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	w := &zeroBurstAfterGo{fakeTPM: f}
	tp, err := Open(w)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrBurstTimeout {
		t.Fatalf("want ErrBurstTimeout, got %v", err)
	}
}

func TestSendBodyReadStsInvalid(t *testing.T) {
	// During the body read, the dataAvail re-check reads an invalid STS
	// (stsValid clear), hitting readFIFO's readSts-failure branch.
	f := newFakeTPM(goodResponse(t, 40))
	f.burst = 4
	cmd := common.BuildCommand(uint16(common.TagNoSessions), uint32(common.CCGetRandom), []byte{0x01})
	w := &invalidStsMidBody{fakeTPM: f, after: common.HeaderSize + 4}
	tp, err := Open(w)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.armExpect(len(cmd))
	if _, err := tp.Send(cmd); err != ErrShortResponse {
		t.Fatalf("want ErrShortResponse, got %v", err)
	}
}

// zeroBurstAfterGo forces burstCount to a permanent zero once tpmGo has
// been written, so the response-read waitBurst times out.
type zeroBurstAfterGo struct {
	*fakeTPM
}

func (w *zeroBurstAfterGo) Read32(off uint32) uint32 {
	if off == regSts && w.fakeTPM.went {
		w.fakeTPM.burst = 0
		w.fakeTPM.burstZeroFor = maxSpins + 10
	}
	return w.fakeTPM.Read32(off)
}

// invalidStsMidBody drops stsValid once `after` FIFO bytes have been
// read, so the body-read dataAvail re-check reads an invalid STS.
type invalidStsMidBody struct {
	*fakeTPM
	after int
}

func (w *invalidStsMidBody) Read32(off uint32) uint32 {
	if off == regSts && w.fakeTPM.respPos >= w.after {
		w.fakeTPM.noValidSts = true
	}
	return w.fakeTPM.Read32(off)
}

func TestErrorString(t *testing.T) {
	// Exercise the Error.Error method.
	if ErrNoLocality.Error() != "tis: timed out claiming locality 0" {
		t.Fatalf("unexpected error text: %q", ErrNoLocality.Error())
	}
}
