// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package zbin_test

import (
	"encoding/binary"
	"os"
	"os/exec"
	"testing"

	"github.com/siderolabs/go-zbin/zbin"
)

const recordLen = 16

// TestOracleLive compares against a live run of the original C tool on
// arbitrary artifacts, for example a real iPXE build. It is skipped unless
// the environment provides:
//
//	ZBIN_ORACLE  path of a zbin binary built from the iPXE source tree
//	ZBIN_BIN     path of a flat binary
//	ZBIN_ZINFO   path of its directive stream
//
// The comparison is structural: uncompressed regions must match byte for
// byte outside patched size fields, and the compressed streams of both
// outputs must decode back to the identical filtered input.
func TestOracleLive(t *testing.T) {
	oracle := os.Getenv("ZBIN_ORACLE")
	binPath := os.Getenv("ZBIN_BIN")
	zinfoPath := os.Getenv("ZBIN_ZINFO")

	if oracle == "" || binPath == "" || zinfoPath == "" {
		t.Skip("ZBIN_ORACLE, ZBIN_BIN and ZBIN_ZINFO are not all set")
	}

	bin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}

	zinfo, err := os.ReadFile(zinfoPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), oracle, binPath, zinfoPath)
	cmd.Stderr = os.Stderr

	golden, err := cmd.Output()
	if err != nil {
		t.Fatalf("oracle failed: %v", err)
	}

	ours, err := zbin.Compress(bin, zinfo)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("input %d bytes, oracle %d bytes, this module %d bytes (%.2f%%)",
		len(bin), len(golden), len(ours), 100*float64(len(ours))/float64(len(golden)))

	compareStructurally(t, bin, zinfo, golden, ours)
}

// addSpec describes an ADD-family directive: the width of the patched
// field, which length it patches in and what its offset is relative to.
type addSpec struct {
	width      int
	source     byte // 'T' total, 'H' header, 'P' payload
	fromHeader bool
}

// addSpecFor derives the directive semantics from the tag itself: the
// fourth character selects the field width and the first three the length
// source, with the APP variants taking offsets relative to the header end.
func addSpecFor(tag string) (addSpec, bool) {
	var spec addSpec

	switch tag[3] {
	case 'B':
		spec.width = 1
	case 'W':
		spec.width = 2
	case 'L':
		spec.width = 4
	default:
		return spec, false
	}

	switch tag[:3] {
	case "ADD":
		spec.source = 'T'
	case "ADH":
		spec.source = 'H'
	case "ADP":
		spec.source = 'P'
	case "APP":
		spec.source = 'P'
		spec.fromHeader = true
	default:
		return spec, false
	}

	return spec, true
}

// patchedOffsets collects the output offsets written by size patches: those
// bytes legitimately differ between two outputs of different compressors.
func patchedOffsets(t *testing.T, zinfo []byte) map[int]bool {
	t.Helper()

	patched := map[int]bool{}

	for i := 0; i+recordLen <= len(zinfo); i += recordLen {
		spec, ok := addSpecFor(string(zinfo[i : i+4]))
		if !ok {
			continue
		}

		if spec.fromHeader {
			// Header-relative patch offsets need the header mark, which
			// depends on compressed sizes. Resolving them here would
			// duplicate the whole interpreter, so give up on masking and
			// let a mismatch surface if such an image ever shows up.
			t.Fatalf("structural comparison does not support header-relative patches")
		}

		off := int(binary.LittleEndian.Uint32(zinfo[i+4:]))
		for j := range spec.width {
			patched[off+j] = true
		}
	}

	return patched
}

// walkState tracks one output's walk position and header mark.
type walkState struct {
	name string
	buf  []byte
	pos  int
	hdr  int
}

func (w *walkState) align(a uint32) {
	w.pos = (w.pos + int(a) - 1) &^ (int(a) - 1)
}

// checkCopy verifies that a copied region matches the input outside patched
// bytes and advances past it.
func (w *walkState) checkCopy(t *testing.T, rec int, src []byte, patched map[int]bool) {
	t.Helper()

	for j, want := range src {
		if w.buf[w.pos+j] != want && !patched[w.pos+j] {
			t.Fatalf("record %d: %s output differs from the input in a copied region at %#x", rec, w.name, w.pos+j)
		}
	}

	w.pos += len(src)
}

// impliedAddend reads a patched size field and subtracts the scaled length,
// recovering the addend the field held before patching.
func (w *walkState) impliedAddend(spec addSpec, off int, divisor uint32) int64 {
	var length int

	switch spec.source {
	case 'T':
		length = w.pos
	case 'H':
		length = w.hdr
	case 'P':
		length = w.pos - w.hdr
	}

	var val int64

	switch spec.width {
	case 1:
		val = int64(w.buf[off])
	case 2:
		val = int64(binary.LittleEndian.Uint16(w.buf[off:]))
	default:
		val = int64(binary.LittleEndian.Uint32(w.buf[off:]))
	}

	return val - int64((length+int(divisor)-1)/int(divisor))
}

// compareStructurally walks the directive stream against two outputs and
// verifies their equivalence modulo compressed stream bytes.
func compareStructurally(t *testing.T, bin, zinfo, golden, ours []byte) {
	t.Helper()

	patched := patchedOffsets(t, zinfo)

	// Filter the input regions in record order for the reference.
	ref := append([]byte(nil), bin...)

	g := &walkState{buf: golden, name: "oracle"}
	o := &walkState{buf: ours, name: "go"}

	for i := 0; i+recordLen <= len(zinfo); i += recordLen {
		tag := string(zinfo[i : i+4])
		a := binary.LittleEndian.Uint32(zinfo[i+4:])
		b := binary.LittleEndian.Uint32(zinfo[i+8:])
		d := binary.LittleEndian.Uint32(zinfo[i+12:])

		switch tag {
		case "COPY":
			g.align(d)
			o.align(d)
			g.checkCopy(t, i/recordLen, bin[a:int(a)+int(b)], patched)
			o.checkCopy(t, i/recordLen, bin[a:int(a)+int(b)], patched)
		case "PACK":
			g.align(d)
			o.align(d)

			region := ref[a : int(a)+int(b)]
			zbin.BCJFilterX86(region)

			g.pos = checkPackFrame(t, g.name, g.buf, g.pos, region)
			o.pos = checkPackFrame(t, o.name, o.buf, o.pos, region)
		case "PAYL":
			g.align(d)
			o.align(d)
			g.hdr, o.hdr = g.pos, o.pos
		case "BASE", "ZREL":
			t.Fatalf("structural comparison does not support %s", tag)
		default:
			// ADD family: both outputs must imply the same original addend
			// from their own sizes.
			spec, ok := addSpecFor(tag)
			if !ok {
				t.Fatalf("record %d: unknown directive %q", i/recordLen, tag)
			}

			goldenAddend := g.impliedAddend(spec, int(a), b)
			oursAddend := o.impliedAddend(spec, int(a), b)

			if goldenAddend != oursAddend {
				t.Fatalf("record %d (%s): implied addends differ: oracle %d vs %d", i/recordLen, tag, goldenAddend, oursAddend)
			}
		}
	}

	if g.pos != len(golden) || o.pos != len(ours) {
		t.Fatalf("walked %d/%d bytes of outputs sized %d/%d", g.pos, o.pos, len(golden), len(ours))
	}
}
