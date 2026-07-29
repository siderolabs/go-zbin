// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package zbin

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ELF machine and relocation type identifiers handled by ZREL. Only RISC-V
// is defined by the format so far.
const (
	elfMachineRISCV = 243

	relocRISCVNone     = 0
	relocRISCVRelative = 3
)

// zrelSkipBits is the width of the skip count in a compressed relocation
// table entry whose most significant bit is clear.
const zrelSkipBits = 19

// zrel reads ELF dynamic relocation entries with explicit addends from an
// input region, applies the relative relocations to the output image and
// appends the compressed relocation table.
//
// Each table entry is one little-endian machine word. If the most
// significant bit is set, the remaining bits form a bitmap of upcoming
// machine words to relocate, one bit per word. If it is clear, the top 19
// bits below it hold a count of machine words to skip and the rest is the
// bitmap. A word with the most significant bit and the whole skip count
// clear terminates the table, after its bitmap is applied.
func (c *compressor) zrel(offset, length, machine uint32) error {
	in, err := c.inputRegion(offset, length)
	if err != nil {
		return err
	}

	wordSize := c.relocWordSize
	if wordSize == 0 {
		return errors.New("relocation word size is not configured, pass WithRelocWordSize")
	}

	n, err := alignUp(c.len, uint32(wordSize))
	if err != nil {
		return err
	}

	if err := c.ensure(n); err != nil {
		return err
	}

	c.len = n

	if machine != elfMachineRISCV {
		return fmt.Errorf("unsupported relocation machine type %d", machine)
	}

	// RISC-V dynamic relocations carry an explicit addend, making each entry
	// three machine words: target offset, info and addend.
	stride := 3 * wordSize

	if len(in)%stride != 0 {
		return fmt.Errorf("region length %d is not a whole number of %d-byte relocation entries", len(in), stride)
	}

	tableStart := c.len
	table := newZrelTable(c)

	var (
		prev    uint64
		hasPrev bool
	)

	for pos := 0; pos < len(in); pos += stride {
		entry := parseRelocEntry(in[pos:], wordSize)

		switch entry.relType(wordSize) {
		case relocRISCVNone:
			continue
		case relocRISCVRelative:
		default:
			return fmt.Errorf("unsupported relocation type %d", entry.relType(wordSize))
		}

		target := entry.offset - c.base

		// The write is wordSize bytes wide and must land entirely inside the
		// output emitted before the relocation table. The subtraction form
		// keeps the comparison safe against uint64 wraparound.
		if uint64(tableStart) < uint64(wordSize) || target > uint64(tableStart)-uint64(wordSize) {
			return fmt.Errorf("relocation target %#x is outside the output", target)
		}

		if target%uint64(wordSize) != 0 {
			return fmt.Errorf("relocation target %#x is not aligned to the %d-byte word size", target, wordSize)
		}

		if hasPrev && target <= prev {
			return fmt.Errorf("relocation target %#x is not sorted after %#x", target, prev)
		}

		prev, hasPrev = target, true

		if wordSize == 4 {
			binary.LittleEndian.PutUint32(c.out[target:], uint32(entry.addend))
		} else {
			binary.LittleEndian.PutUint64(c.out[target:], entry.addend)
		}

		if err := table.mark(target / uint64(wordSize)); err != nil {
			return err
		}
	}

	if err := table.finish(); err != nil {
		return err
	}

	table.writeAt(tableStart)

	return nil
}

// relocEntry is one ELF dynamic relocation entry: three little-endian
// machine words holding the target offset, the relocation info and the
// addend.
type relocEntry struct {
	offset uint64
	info   uint64
	addend uint64
}

func parseRelocEntry(in []byte, wordSize int) relocEntry {
	if wordSize == 4 {
		return relocEntry{
			offset: uint64(binary.LittleEndian.Uint32(in)),
			info:   uint64(binary.LittleEndian.Uint32(in[4:])),
			addend: uint64(binary.LittleEndian.Uint32(in[8:])),
		}
	}

	return relocEntry{
		offset: binary.LittleEndian.Uint64(in),
		info:   binary.LittleEndian.Uint64(in[8:]),
		addend: binary.LittleEndian.Uint64(in[16:]),
	}
}

// relType extracts the relocation type from the info word: the low 8 bits
// in the 32-bit entry layout, the low 32 bits in the 64-bit one.
func (e relocEntry) relType(wordSize int) uint64 {
	if wordSize == 4 {
		return e.info & 0xff
	}

	return e.info & 0xffffffff
}

// zrelTable accumulates the compressed relocation table entries and
// accounts for the output space they take as they are opened.
type zrelTable struct {
	c       *compressor
	entries []uint64

	noSkipShift int
	skipShift   int
	skipMax     uint64
	wordBase    uint64
	limit       uint64
}

func newZrelTable(c *compressor) *zrelTable {
	noSkipShift := 8*c.relocWordSize - 1

	return &zrelTable{
		c:           c,
		noSkipShift: noSkipShift,
		skipShift:   noSkipShift - zrelSkipBits,
		skipMax:     uint64(1)<<zrelSkipBits - 1,
	}
}

// mark records that the machine word at the given index needs relocating,
// opening new table entries as needed.
func (t *zrelTable) mark(wordIndex uint64) error {
	delta := wordIndex - t.wordBase

	for delta >= t.limit {
		if err := t.grow(); err != nil {
			return err
		}

		t.wordBase += t.limit
		delta -= t.limit

		switch {
		case delta < zrelSkipBits:
			// A short gap is cheaper to express as bitmap zeroes than by
			// starting a skip entry.
			t.entries[len(t.entries)-1] = uint64(1) << t.noSkipShift
			t.limit = uint64(t.noSkipShift)
		case delta <= t.skipMax:
			t.entries[len(t.entries)-1] = delta << t.skipShift
			t.wordBase += delta
			delta = 0
			t.limit = uint64(t.skipShift)
		default:
			t.entries[len(t.entries)-1] = t.skipMax << t.skipShift
			t.wordBase += t.skipMax
			delta -= t.skipMax
			t.limit = uint64(t.skipShift)
		}
	}

	t.entries[len(t.entries)-1] |= uint64(1) << delta

	return nil
}

// grow opens a new all-zero table entry, accounting for its output space.
func (t *zrelTable) grow() error {
	if err := t.c.ensure(t.c.len + t.c.relocWordSize); err != nil {
		return err
	}

	t.c.len += t.c.relocWordSize
	t.entries = append(t.entries, 0)

	return nil
}

// finish terminates the table: the final entry doubles as the terminator
// when its skip count is zero, otherwise a fresh all-zero entry is added.
func (t *zrelTable) finish() error {
	if len(t.entries) > 0 && t.entries[len(t.entries)-1]&(t.skipMax<<t.skipShift) == 0 {
		t.entries[len(t.entries)-1] &^= uint64(1) << t.noSkipShift

		return nil
	}

	return t.grow()
}

// writeAt serializes the table entries into the output buffer.
func (t *zrelTable) writeAt(start int) {
	for i, entry := range t.entries {
		if t.c.relocWordSize == 4 {
			binary.LittleEndian.PutUint32(t.c.out[start+i*4:], uint32(entry))
		} else {
			binary.LittleEndian.PutUint64(t.c.out[start+i*8:], entry)
		}
	}
}
