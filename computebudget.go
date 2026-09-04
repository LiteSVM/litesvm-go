package litesvm

import (
	"fmt"

	"github.com/sonicfromnewyoke/mithril/pkg/sealevel"
)

// ComputeBudget mirrors litesvm-go's ComputeBudget (itself a mirror of
// solana_compute_budget::compute_budget::ComputeBudget). Fields typed as
// `usize` on the Solana side are surfaced as uint64; HeapSize stays uint32.
type ComputeBudget struct {
	ComputeUnitLimit                      uint64
	Log64Units                            uint64
	CreateProgramAddressUnits             uint64
	InvokeUnits                           uint64
	MaxInstructionStackDepth              uint64
	MaxInstructionTraceLength             uint64
	Sha256BaseCost                        uint64
	Sha256ByteCost                        uint64
	Sha256MaxSlices                       uint64
	MaxCallDepth                          uint64
	StackFrameSize                        uint64
	LogPubkeyUnits                        uint64
	CpiBytesPerUnit                       uint64
	SysvarBaseCost                        uint64
	Secp256k1RecoverCost                  uint64
	SyscallBaseCost                       uint64
	Curve25519EdwardsValidatePointCost    uint64
	Curve25519EdwardsAddCost              uint64
	Curve25519EdwardsSubtractCost         uint64
	Curve25519EdwardsMultiplyCost         uint64
	Curve25519EdwardsMsmBaseCost          uint64
	Curve25519EdwardsMsmIncrementalCost   uint64
	Curve25519RistrettoValidatePointCost  uint64
	Curve25519RistrettoAddCost            uint64
	Curve25519RistrettoSubtractCost       uint64
	Curve25519RistrettoMultiplyCost       uint64
	Curve25519RistrettoMsmBaseCost        uint64
	Curve25519RistrettoMsmIncrementalCost uint64
	HeapSize                              uint32
	HeapCost                              uint64
	MemOpBaseCost                         uint64
	AltBn128AdditionCost                  uint64
	AltBn128MultiplicationCost            uint64
	AltBn128PairingOnePairCostFirst       uint64
	AltBn128PairingOnePairCostOther       uint64
	BigModularExponentiationBaseCost      uint64
	BigModularExponentiationCostDivisor   uint64
	PoseidonCostCoefficientA              uint64
	PoseidonCostCoefficientC              uint64
	GetRemainingComputeUnitsCost          uint64
	AltBn128G1Compress                    uint64
	AltBn128G1Decompress                  uint64
	AltBn128G2Compress                    uint64
	AltBn128G2Decompress                  uint64
}

// DefaultComputeBudget returns the Agave runtime defaults
// (solana-compute-budget 4.0.0, ComputeBudget::new_with_defaults with
// SIMD-0268 and SIMD-0339 inactive). Mithril's cu package constants match
// these values. The AltBn128AdditionCost / AltBn128MultiplicationCost
// fields carry the G1 costs.
func DefaultComputeBudget() ComputeBudget {
	return ComputeBudget{
		ComputeUnitLimit:                      1_400_000,
		Log64Units:                            100,
		CreateProgramAddressUnits:             1500,
		InvokeUnits:                           1000,
		MaxInstructionStackDepth:              5,
		MaxInstructionTraceLength:             64,
		Sha256BaseCost:                        85,
		Sha256ByteCost:                        1,
		Sha256MaxSlices:                       20_000,
		MaxCallDepth:                          64,
		StackFrameSize:                        4096,
		LogPubkeyUnits:                        100,
		CpiBytesPerUnit:                       250,
		SysvarBaseCost:                        100,
		Secp256k1RecoverCost:                  25_000,
		SyscallBaseCost:                       100,
		Curve25519EdwardsValidatePointCost:    159,
		Curve25519EdwardsAddCost:              473,
		Curve25519EdwardsSubtractCost:         475,
		Curve25519EdwardsMultiplyCost:         2_177,
		Curve25519EdwardsMsmBaseCost:          2_273,
		Curve25519EdwardsMsmIncrementalCost:   758,
		Curve25519RistrettoValidatePointCost:  169,
		Curve25519RistrettoAddCost:            521,
		Curve25519RistrettoSubtractCost:       519,
		Curve25519RistrettoMultiplyCost:       2_208,
		Curve25519RistrettoMsmBaseCost:        2_303,
		Curve25519RistrettoMsmIncrementalCost: 788,
		HeapSize:                              32 * 1024,
		HeapCost:                              8,
		MemOpBaseCost:                         10,
		AltBn128AdditionCost:                  334,
		AltBn128MultiplicationCost:            3_840,
		AltBn128PairingOnePairCostFirst:       36_364,
		AltBn128PairingOnePairCostOther:       12_121,
		BigModularExponentiationBaseCost:      190,
		BigModularExponentiationCostDivisor:   2,
		PoseidonCostCoefficientA:              61,
		PoseidonCostCoefficientC:              542,
		GetRemainingComputeUnitsCost:          100,
		AltBn128G1Compress:                    30,
		AltBn128G1Decompress:                  398,
		AltBn128G2Compress:                    86,
		AltBn128G2Decompress:                  13_610,
	}
}

// ComputeBudget returns the current custom compute budget. The bool is
// false (with a zero-value budget) when no custom budget has been
// configured and the runtime default is in effect.
func (s *LiteSVM) ComputeBudget() (ComputeBudget, bool, error) {
	if s.computeBudget == nil {
		return ComputeBudget{}, false, nil
	}
	return *s.computeBudget, true, nil
}

// SetComputeBudget replaces the compute budget used for subsequent txs.
//
// Mithril's pure pipeline (replay.LoadAndExecuteTransaction) derives its
// compute budget solely from the transaction's compute-budget instructions,
// so only the knobs that can be expressed as such instructions are honored
// per-instance:
//
//   - ComputeUnitLimit and HeapSize are threaded into execution by
//     appending synthetic compute-budget instructions to transactions that
//     carry none of their own (see tx.go). Transactions with explicit
//     compute-budget instructions keep their own budget, and the injected
//     instructions consume the usual 150 CU each. ComputeUnitLimit must not
//     exceed 1_400_000, and HeapSize must be a multiple of 1024 within
//     [32KiB, 256KiB]: the runtime rejects other values.
//   - MaxInstructionStackDepth and MaxInstructionTraceLength are hardcoded
//     by mithril's replay package (5 and 64, the Agave defaults) and can
//     only be set to those values.
//
// Every other field is a VM cost constant baked into mithril's cu package;
// setting it to anything but the default is an error naming the field.
func (s *LiteSVM) SetComputeBudget(b ComputeBudget) error {
	def := DefaultComputeBudget()

	if b.ComputeUnitLimit > sealevel.MaxComputeUnitLimit {
		return fmt.Errorf("%w: SetComputeBudget: ComputeUnitLimit %d exceeds the %d cap mithril enforces",
			ErrLiteSVM, b.ComputeUnitLimit, uint64(sealevel.MaxComputeUnitLimit))
	}
	if b.HeapSize < sealevel.MinHeapFrameBytes || b.HeapSize > sealevel.MaxHeapFrameBytes ||
		b.HeapSize%sealevel.HeapFrameBytesMultiple != 0 {
		return fmt.Errorf("%w: SetComputeBudget: HeapSize %d is not a multiple of 1024 in [%d, %d]",
			ErrLiteSVM, b.HeapSize, uint64(sealevel.MinHeapFrameBytes), uint64(sealevel.MaxHeapFrameBytes))
	}
	if b.MaxInstructionStackDepth != def.MaxInstructionStackDepth {
		return fmt.Errorf("%w: SetComputeBudget: MaxInstructionStackDepth is hardcoded to %d by mithril's replay package (got %d)",
			ErrLiteSVM, def.MaxInstructionStackDepth, b.MaxInstructionStackDepth)
	}
	if b.MaxInstructionTraceLength != def.MaxInstructionTraceLength {
		return fmt.Errorf("%w: SetComputeBudget: MaxInstructionTraceLength is hardcoded to %d by mithril's replay package (got %d)",
			ErrLiteSVM, def.MaxInstructionTraceLength, b.MaxInstructionTraceLength)
	}

	// All remaining fields are execution cost constants mithril does not
	// expose per-instance; reject any deviation from the defaults.
	fixed := []struct {
		name      string
		got, want uint64
	}{
		{"Log64Units", b.Log64Units, def.Log64Units},
		{"CreateProgramAddressUnits", b.CreateProgramAddressUnits, def.CreateProgramAddressUnits},
		{"InvokeUnits", b.InvokeUnits, def.InvokeUnits},
		{"Sha256BaseCost", b.Sha256BaseCost, def.Sha256BaseCost},
		{"Sha256ByteCost", b.Sha256ByteCost, def.Sha256ByteCost},
		{"Sha256MaxSlices", b.Sha256MaxSlices, def.Sha256MaxSlices},
		{"MaxCallDepth", b.MaxCallDepth, def.MaxCallDepth},
		{"StackFrameSize", b.StackFrameSize, def.StackFrameSize},
		{"LogPubkeyUnits", b.LogPubkeyUnits, def.LogPubkeyUnits},
		{"CpiBytesPerUnit", b.CpiBytesPerUnit, def.CpiBytesPerUnit},
		{"SysvarBaseCost", b.SysvarBaseCost, def.SysvarBaseCost},
		{"Secp256k1RecoverCost", b.Secp256k1RecoverCost, def.Secp256k1RecoverCost},
		{"SyscallBaseCost", b.SyscallBaseCost, def.SyscallBaseCost},
		{"Curve25519EdwardsValidatePointCost", b.Curve25519EdwardsValidatePointCost, def.Curve25519EdwardsValidatePointCost},
		{"Curve25519EdwardsAddCost", b.Curve25519EdwardsAddCost, def.Curve25519EdwardsAddCost},
		{"Curve25519EdwardsSubtractCost", b.Curve25519EdwardsSubtractCost, def.Curve25519EdwardsSubtractCost},
		{"Curve25519EdwardsMultiplyCost", b.Curve25519EdwardsMultiplyCost, def.Curve25519EdwardsMultiplyCost},
		{"Curve25519EdwardsMsmBaseCost", b.Curve25519EdwardsMsmBaseCost, def.Curve25519EdwardsMsmBaseCost},
		{"Curve25519EdwardsMsmIncrementalCost", b.Curve25519EdwardsMsmIncrementalCost, def.Curve25519EdwardsMsmIncrementalCost},
		{"Curve25519RistrettoValidatePointCost", b.Curve25519RistrettoValidatePointCost, def.Curve25519RistrettoValidatePointCost},
		{"Curve25519RistrettoAddCost", b.Curve25519RistrettoAddCost, def.Curve25519RistrettoAddCost},
		{"Curve25519RistrettoSubtractCost", b.Curve25519RistrettoSubtractCost, def.Curve25519RistrettoSubtractCost},
		{"Curve25519RistrettoMultiplyCost", b.Curve25519RistrettoMultiplyCost, def.Curve25519RistrettoMultiplyCost},
		{"Curve25519RistrettoMsmBaseCost", b.Curve25519RistrettoMsmBaseCost, def.Curve25519RistrettoMsmBaseCost},
		{"Curve25519RistrettoMsmIncrementalCost", b.Curve25519RistrettoMsmIncrementalCost, def.Curve25519RistrettoMsmIncrementalCost},
		{"HeapCost", b.HeapCost, def.HeapCost},
		{"MemOpBaseCost", b.MemOpBaseCost, def.MemOpBaseCost},
		{"AltBn128AdditionCost", b.AltBn128AdditionCost, def.AltBn128AdditionCost},
		{"AltBn128MultiplicationCost", b.AltBn128MultiplicationCost, def.AltBn128MultiplicationCost},
		{"AltBn128PairingOnePairCostFirst", b.AltBn128PairingOnePairCostFirst, def.AltBn128PairingOnePairCostFirst},
		{"AltBn128PairingOnePairCostOther", b.AltBn128PairingOnePairCostOther, def.AltBn128PairingOnePairCostOther},
		{"BigModularExponentiationBaseCost", b.BigModularExponentiationBaseCost, def.BigModularExponentiationBaseCost},
		{"BigModularExponentiationCostDivisor", b.BigModularExponentiationCostDivisor, def.BigModularExponentiationCostDivisor},
		{"PoseidonCostCoefficientA", b.PoseidonCostCoefficientA, def.PoseidonCostCoefficientA},
		{"PoseidonCostCoefficientC", b.PoseidonCostCoefficientC, def.PoseidonCostCoefficientC},
		{"GetRemainingComputeUnitsCost", b.GetRemainingComputeUnitsCost, def.GetRemainingComputeUnitsCost},
		{"AltBn128G1Compress", b.AltBn128G1Compress, def.AltBn128G1Compress},
		{"AltBn128G1Decompress", b.AltBn128G1Decompress, def.AltBn128G1Decompress},
		{"AltBn128G2Compress", b.AltBn128G2Compress, def.AltBn128G2Compress},
		{"AltBn128G2Decompress", b.AltBn128G2Decompress, def.AltBn128G2Decompress},
	}
	for _, f := range fixed {
		if f.got != f.want {
			return fmt.Errorf("%w: SetComputeBudget: %s is a fixed mithril cost constant (%d) and cannot be set to %d",
				ErrLiteSVM, f.name, f.want, f.got)
		}
	}

	cb := b
	s.computeBudget = &cb
	return nil
}
