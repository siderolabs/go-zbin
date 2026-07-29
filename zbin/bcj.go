// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package zbin

import "encoding/binary"

const (
	opcodeCall = 0xe8
	opcodeJmp  = 0xe9
)

// bcjFilterX86 applies the branch conversion filter the format defines for
// x86 code, in place. Operands of near call and jmp instructions are
// rewritten from relative to absolute addresses, which makes repeated calls
// to the same target byte-identical and therefore more compressible.
//
// A block position can only be rewritten when the resulting absolute address
// stays inside the block, since the reverse filter in the decompressor has
// only the block length to decide which values were rewritten. Relative
// values that were not rewritten but happen to fall into the absolute range
// would collide with rewritten ones, so those are shifted down below zero
// into the range that rewriting has vacated, keeping the mapping reversible.
func bcjFilterX86(data []byte) {
	limit := len(data) - 5

	for off := 0; off <= limit; off++ {
		if data[off] != opcodeCall && data[off] != opcodeJmp {
			continue
		}

		target := int(int32(binary.LittleEndian.Uint32(data[off+1:])))

		switch {
		case target >= -off && target < limit-off:
			// Relative targets resolving to [0,limit) become absolute.
			target += off
		case target >= limit-off && target < limit:
			// Colliding untouched values move to [-off,0).
			target -= limit
		}

		binary.LittleEndian.PutUint32(data[off+1:], uint32(int32(target)))

		// The operand bytes are never scanned for opcodes themselves.
		off += 4
	}
}
