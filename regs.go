// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/tis authors. All rights reserved.

package tis

// Register offsets and bit definitions for the TCG PC Client TIS / FIFO
// interface.
//
// Authority: TCG "PC Client Platform TPM Profile (PTP) Specification",
// the "FIFO Interface" chapter (which folds in the legacy TCG "PC Client
// Specific TPM Interface Specification (TIS)" version 1.3). Offsets are
// byte offsets within a locality's 4 KiB register page; this driver
// targets locality 0, whose page conventionally begins at physical
// 0xFED4_0000. The platform owns the actual mapping and presents the
// page through common.Regs, so this package works purely in offsets.
//
// The control/status registers below are *native-endian* MMIO accesses
// via common.Regs (Read8/Read32/Write8/Write32). Only the command and
// response payloads streamed through DATA_FIFO carry the big-endian
// TPM 2.0 wire encoding, and those are byte streams (no endianness of
// their own at the FIFO).

// Register offsets within the locality-0 page. PTP "FIFO Interface
// Locality ... Register Space" / TIS 1.3 Table "Register Space".
const (
	regAccess      uint32 = 0x00  // TPM_ACCESS_x         (1 byte)
	regIntEnable   uint32 = 0x08  // TPM_INT_ENABLE_x     (4 bytes)
	regIntVector   uint32 = 0x0C  // TPM_INT_VECTOR_x     (1 byte)
	regIntStatus   uint32 = 0x10  // TPM_INT_STATUS_x     (4 bytes)
	regIntfCap     uint32 = 0x14  // TPM_INTF_CAPABILITY_x(4 bytes)
	regSts         uint32 = 0x18  // TPM_STS_x            (4 bytes)
	regDataFIFO    uint32 = 0x24  // TPM_DATA_FIFO_x      (1 byte, streamed)
	regInterfaceID uint32 = 0x30  // TPM_INTERFACE_ID_x  (4 bytes)
	regDIDVID      uint32 = 0xF00 // TPM_DID_VID_x        (4 bytes)
	regRID         uint32 = 0xF04 // TPM_RID_x            (1 byte)
)

// TPM_ACCESS_x bit fields. PTP "TPM_ACCESS_x" register table.
const (
	accessTPMRegValidSts uint8 = 1 << 7 // b7 tpmRegValidSts: register bits are valid.
	accessActiveLocality uint8 = 1 << 5 // b5 activeLocality: this locality is active (claimed).
	accessBeenSeized     uint8 = 1 << 4 // b4 beenSeized: locality was seized by a higher one.
	accessSeize          uint8 = 1 << 3 // b3 Seize: write 1 to seize from a lower locality.
	accessPendingRequest uint8 = 1 << 2 // b2 pendingRequest: another locality wants access.
	accessRequestUse     uint8 = 1 << 1 // b1 requestUse: write 1 to request this locality.
	accessTPMEstablish   uint8 = 1 << 0 // b0 tpmEstablishment: 0 = establishment asserted.
)

// TPM_STS_x bit fields. PTP "TPM_STS_x" register table.
const (
	stsValid         uint32 = 1 << 7 // b7 stsValid: stsValid, Expect and dataAvail are valid.
	stsCommandReady  uint32 = 1 << 6 // b6 commandReady: TPM is ready to receive a command.
	stsTPMGo         uint32 = 1 << 5 // b5 tpmGo: write 1 to start command execution.
	stsDataAvail     uint32 = 1 << 4 // b4 dataAvail: response data is available in the FIFO.
	stsExpect        uint32 = 1 << 3 // b3 Expect: TPM still expects more command bytes.
	stsSelfTestDone  uint32 = 1 << 2 // b2 selfTestDone: TPM self-test has completed.
	stsResponseRetry uint32 = 1 << 1 // b1 responseRetry: write 1 to re-send the response.
)

// burstCount occupies TPM_STS_x bits 8..23 (16 bits): the number of
// bytes the TPM can accept (write) or has ready (read) without polling.
// PTP "TPM_STS_x", burstCount field.
const (
	stsBurstShift = 8
	stsBurstMask  = 0xFFFF
)

// burstCount extracts the burstCount field (bits 8..23) from a TPM_STS_x
// value. PTP "TPM_STS_x", burstCount field.
func burstCount(sts uint32) int {
	return int((sts >> stsBurstShift) & stsBurstMask)
}
