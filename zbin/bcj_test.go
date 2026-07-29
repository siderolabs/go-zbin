// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package zbin_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/siderolabs/go-zbin/zbin"
)

// bcjUnfilterX86 is the inverse transformation, as applied by the
// decompressor embedded in the images. It exists here to prove the filter
// reversible with only the block length as context.
func bcjUnfilterX86(data []byte) {
	limit := len(data) - 5

	for off := 0; off <= limit; off++ {
		if data[off] != 0xe8 && data[off] != 0xe9 {
			continue
		}

		target := int(int32(binary.LittleEndian.Uint32(data[off+1:])))

		switch {
		case target >= 0 && target < limit:
			target -= off
		case target >= -off && target < 0:
			target += limit
		}

		binary.LittleEndian.PutUint32(data[off+1:], uint32(int32(target)))

		off += 4
	}
}

func putJump(buf []byte, off int, opcode byte, target int32) {
	buf[off] = opcode
	binary.LittleEndian.PutUint32(buf[off+1:], uint32(target))
}

func jumpTarget(buf []byte, off int) int32 {
	return int32(binary.LittleEndian.Uint32(buf[off+1:]))
}

func TestBCJVectors(t *testing.T) {
	// A 20-byte block has limit 15.
	mk := func() []byte {
		buf := make([]byte, 20)
		for i := range buf {
			buf[i] = 0x90
		}

		return buf
	}

	for _, tc := range []struct {
		name   string
		off    int
		opcode byte
		target int32
		want   int32
	}{
		{"relative to absolute, lowest", 2, 0xe8, -2, 0},
		{"relative to absolute, mid", 2, 0xe9, 5, 7},
		{"relative to absolute, highest", 2, 0xe8, 12, 14},
		{"colliding, first", 2, 0xe8, 13, -2},
		{"colliding, last", 2, 0xe8, 14, -1},
		{"above range, unchanged", 2, 0xe8, 15, 15},
		{"below range, unchanged", 2, 0xe9, -3, -3},
		{"far positive, unchanged", 2, 0xe8, 1 << 28, 1 << 28},
		{"far negative, unchanged", 2, 0xe8, -(1 << 28), -(1 << 28)},
		// An opcode at the last scannable offset (off == limit) must still
		// be processed: these fail if the scan bound regresses to off < limit.
		{"at last offset, rewritten", 15, 0xe8, -15, 0},
		{"at last offset, colliding", 15, 0xe9, 14, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := mk()
			putJump(buf, tc.off, tc.opcode, tc.target)

			zbin.BCJFilterX86(buf)

			if got := jumpTarget(buf, tc.off); got != tc.want {
				t.Fatalf("target %d became %d, want %d", tc.target, got, tc.want)
			}

			bcjUnfilterX86(buf)

			if got := jumpTarget(buf, tc.off); got != tc.target {
				t.Fatalf("inverse gave %d, want the original %d", got, tc.target)
			}
		})
	}
}

func TestBCJSkipsDisplacementBytes(t *testing.T) {
	buf := make([]byte, 20)
	for i := range buf {
		buf[i] = 0x90
	}

	// The displacement of this jump starts with a branch opcode byte, which
	// must not be interpreted as an instruction. A scanner that wrongly
	// descended into the displacement would find an in-range target there
	// and modify the block. The real value is far outside the block and
	// stays unchanged.
	putJump(buf, 0, 0xe8, 0x000005e8)
	buf[5] = 0

	orig := append([]byte(nil), buf...)

	zbin.BCJFilterX86(buf)

	if !bytes.Equal(buf, orig) {
		t.Fatal("filter modified a block whose only branch has an out-of-range displacement")
	}
}

func TestBCJShortBlocks(t *testing.T) {
	// Blocks up to 4 bytes are never scanned. A 5-byte block is scanned at
	// offset 0, but both rewrite ranges are empty there, so it stays intact.
	for size := range 6 {
		buf := bytes.Repeat([]byte{0xe8}, size)
		orig := append([]byte(nil), buf...)

		zbin.BCJFilterX86(buf)

		if !bytes.Equal(buf, orig) {
			t.Fatalf("filter modified a %d-byte block", size)
		}
	}
}

// FuzzBCJRoundtrip attacks the filter's core property: with only the block
// length as context, the inverse must restore any block exactly. The
// decompressor in the images applies that inverse, so a violation means a
// corrupted boot image.
func FuzzBCJRoundtrip(f *testing.F) {
	f.Add([]byte{0xe8, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0xe9, 0xfb, 0xff, 0xff, 0xff, 0x90, 0x90, 0x90, 0x90, 0x90})
	f.Add(bytes.Repeat([]byte{0xe8, 0xe9}, 32))

	f.Fuzz(func(t *testing.T, data []byte) {
		orig := bytes.Clone(data)

		zbin.BCJFilterX86(data)
		bcjUnfilterX86(data)

		if !bytes.Equal(data, orig) {
			t.Fatal("filter and inverse do not restore the block")
		}
	})
}

func TestBCJInverseOnRandomData(t *testing.T) {
	r := rng(0xdeadbeefcafe)

	for _, size := range []int{5, 6, 100, 4096, 65536} {
		buf := make([]byte, size)
		r.fill(buf)

		// Raise the branch opcode density.
		for i := 0; i+5 < len(buf); i += 13 {
			switch buf[i] % 3 {
			case 0:
				buf[i] = 0xe8
			case 1:
				buf[i] = 0xe9
			}
		}

		orig := append([]byte(nil), buf...)

		zbin.BCJFilterX86(buf)
		bcjUnfilterX86(buf)

		if !bytes.Equal(buf, orig) {
			t.Fatalf("filter and inverse do not restore a %d-byte block", size)
		}
	}
}
