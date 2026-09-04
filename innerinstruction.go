package litesvm

import (
	"github.com/gagliardetto/solana-go"
)

// InnerInstruction is one cross-program invocation (CPI) recorded while a
// transaction executed, together with the depth at which it ran.
//
// This is a port of the upstream anza-xyz/solana-sdk
// `message::inner_instruction::InnerInstruction` struct. It complements
// rpc.InnerInstruction (which is shaped for JSON-RPC responses) as the plain
// core-SDK representation, reusing solana-go's CompiledInstruction.
//
// NOTE: this is a data-model port only; it is NOT bincode-layout-compatible
// with the Rust struct. Reflection-based bin encoding would write StackHeight
// as a little-endian u16 (Rust serializes a u8) and CompiledInstruction's
// widened uint16 indices instead of u8 indices with short-vec lengths.
type InnerInstruction struct {
	// Instruction is the compiled instruction that was invoked. Its account
	// and program indices point into the enclosing transaction message's
	// account-keys table.
	Instruction solana.CompiledInstruction `json:"instruction"`

	// StackHeight is the invocation stack height of this instruction: the
	// runtime's TRANSACTION_LEVEL_STACK_HEIGHT (1) is a top-level
	// instruction, 2 and deeper are CPIs.
	// NOTE: it is actually a uint8, but using a uint16 for consistency with
	// how solana-go widens u8 message indices.
	StackHeight uint16 `json:"stackHeight"`
}

// InnerInstructionsList groups the recorded CPIs of one transaction: the
// outer slice is indexed by top-level instruction index, each inner slice
// holds the CPIs that instruction triggered, in execution order.
//
// Port of solana-sdk `message::inner_instruction::InnerInstructionsList`.
type InnerInstructionsList [][]InnerInstruction
