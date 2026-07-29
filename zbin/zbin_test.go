// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package zbin_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ulikunitz/xz/lzma"

	"github.com/siderolabs/go-zbin/zbin"
)

func readFixture(t testing.TB, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(name, ".gz") {
		return data
	}

	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// rawLZMA1Decode decodes a headerless LZMA1 stream with the fixed properties
// of the format by prepending a synthetic container header. With sizeHint
// set to a non-negative value the decoder stops after that many bytes. With
// a negative sizeHint the header marks the size unknown, so the decode only
// succeeds if the stream carries the end-of-payload marker, exactly like the
// decompressor embedded in the images.
func rawLZMA1Decode(t *testing.T, stream []byte, sizeHint int) []byte {
	t.Helper()

	size := ^uint64(0)
	if sizeHint >= 0 {
		size = uint64(sizeHint)
	}

	hdr := make([]byte, 13)
	hdr[0] = 2 // (pb*5+lp)*9+lc for lc=2, lp=0, pb=0
	binary.LittleEndian.PutUint32(hdr[1:], 1<<26)
	binary.LittleEndian.PutUint64(hdr[5:], size)

	r, err := lzma.ReaderConfig{DictCap: 1 << 26}.NewReader(bytes.NewReader(slices.Concat(hdr, stream)))
	if err != nil {
		t.Fatalf("failed to create LZMA reader: %v", err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to decode LZMA stream: %v", err)
	}

	return out
}

// mustRec builds a single zinfo record.
func mustRec(tag string, a, b, c uint32) []byte {
	out := make([]byte, 16)
	copy(out, tag)
	binary.LittleEndian.PutUint32(out[4:], a)
	binary.LittleEndian.PutUint32(out[8:], b)
	binary.LittleEndian.PutUint32(out[12:], c)

	return out
}

func records(recs ...[]byte) []byte {
	var out []byte
	for _, r := range recs {
		out = append(out, r...)
	}

	return out
}

func TestGoldenCopyAdd(t *testing.T) {
	bin := readFixture(t, "copyadd.bin")
	zinfo := readFixture(t, "copyadd.zinfo")
	golden := readFixture(t, "copyadd.golden")

	out, err := zbin.Compress(bin, zinfo)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(out, golden) {
		t.Fatalf("output differs from the reference tool output: len %d vs %d", len(out), len(golden))
	}
}

func TestGoldenZrel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wordSize int
		gz       bool
	}{
		{"zrel32", 4, false},
		{"zrel64", 8, false},
		{"zrelbig32", 4, true},
		{"zreldense64", 8, false},
		{"zreltie32", 4, false},
		{"zrelgap32", 4, false},
		{"zreledge32", 4, false},
		{"zrelempty64", 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := ""
			if tc.gz {
				suffix = ".gz"
			}

			bin := readFixture(t, tc.name+".bin"+suffix)
			zinfo := readFixture(t, tc.name+".zinfo")
			golden := readFixture(t, tc.name+".golden"+suffix)

			out, err := zbin.Compress(bin, zinfo, zbin.WithRelocWordSize(tc.wordSize))
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(out, golden) {
				t.Fatalf("output differs from the reference tool output: len %d vs %d", len(out), len(golden))
			}
		})
	}
}

// TestGoldenPack verifies the compressed fixture structurally: compressed
// bytes differ between encoders, but both outputs must decode back to the
// identical filtered input and carry consistent framing and size patches.
// The fixture's second COPY overlaps the first packed region, pinning that
// the filter transformation is visible to later directives.
func TestGoldenPack(t *testing.T) {
	bin := readFixture(t, "pack.bin")
	zinfo := readFixture(t, "pack.zinfo")
	golden := readFixture(t, "pack.golden")

	binBefore := bytes.Clone(bin)

	out, err := zbin.Compress(bin, zinfo)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(bin, binBefore) {
		t.Fatal("Compress modified the input binary")
	}

	// Reference BCJ transformation, applied in record order like the
	// compressor does.
	ref := bytes.Clone(bin)
	zbin.BCJFilterX86(ref[512 : 512+3000])
	zbin.BCJFilterX86(ref[4096 : 4096+4096])

	for _, o := range []struct {
		name string
		buf  []byte
	}{
		{"golden", golden},
		{"go", out},
	} {
		// COPY 0,256,16: identical to the input except the ADDW field.
		for i := range 256 {
			if i == 0x40 || i == 0x41 {
				continue
			}

			if o.buf[i] != bin[i] {
				t.Fatalf("%s: copied region differs from input at %#x", o.name, i)
			}
		}

		// PACK 512,3000,16 at position 256.
		pos := checkPackFrame(t, o.name, o.buf, 256, ref[512:512+3000])

		// COPY 512,100,16 overlaps the packed region and must therefore
		// copy the filtered bytes, not the original input.
		pos = (pos + 15) &^ 15
		if !bytes.Equal(o.buf[pos:pos+100], ref[512:612]) {
			t.Fatalf("%s: copy overlapping a packed region does not hold the filtered bytes", o.name)
		}

		copyEnd := pos + 100

		// PAYL aligns to 512.
		pos = (copyEnd + 511) &^ 511

		for _, gap := range o.buf[copyEnd:pos] {
			if gap != 0xff {
				t.Fatalf("%s: alignment gap does not hold 0xff", o.name)
			}
		}

		// PACK 4096,4096,16.
		pos = (pos + 15) &^ 15
		pos = checkPackFrame(t, o.name, o.buf, pos, ref[4096:4096+4096])

		if pos != len(o.buf) {
			t.Fatalf("%s: walked %d bytes, output has %d", o.name, pos, len(o.buf))
		}

		// ADDW 0x40,16: the field holds the total length in 16-byte units,
		// rounded up, on top of a zero addend.
		want := uint16((len(o.buf) + 15) / 16)
		if got := binary.LittleEndian.Uint16(o.buf[0x40:]); got != want {
			t.Fatalf("%s: size patch holds %#x, want %#x", o.name, got, want)
		}
	}
}

// checkPackFrame validates a single compressed frame at pos and returns the
// position after it. The stream must decode to want both with a known size
// and in marker-terminated mode, proving the mandatory end-of-payload
// marker is present.
func checkPackFrame(t *testing.T, name string, buf []byte, pos int, want []byte) int {
	t.Helper()

	frameLen := int(binary.LittleEndian.Uint32(buf[pos:]))
	stream := buf[pos+4 : pos+4+frameLen-4]
	crc := binary.LittleEndian.Uint32(buf[pos+4+len(stream):])

	if got := zbin.PackCRC(stream); got != crc {
		t.Fatalf("%s: frame at %#x has checksum %#x, computed %#x", name, pos, crc, got)
	}

	if dec := rawLZMA1Decode(t, stream, len(want)); !bytes.Equal(dec, want) {
		t.Fatalf("%s: frame at %#x does not decode to the filtered input", name, pos)
	}

	if dec := rawLZMA1Decode(t, stream, -1); !bytes.Equal(dec, want) {
		t.Fatalf("%s: frame at %#x does not terminate on the end-of-payload marker", name, pos)
	}

	return pos + 4 + frameLen
}

// TestRoundtrip compresses in-memory data and reverses the whole pipeline,
// including the branch filter, without any fixture.
func TestRoundtrip(t *testing.T) {
	r := rng(42)
	bin := make([]byte, 10000)
	r.fill(bin)

	// Sprinkle branch opcodes so the filter has work to do.
	for i := 100; i+5 < len(bin); i += 97 {
		bin[i] = 0xe8
	}

	zinfo := records(mustRec("PACK", 0, uint32(len(bin)), 1))

	out, err := zbin.Compress(bin, zinfo)
	if err != nil {
		t.Fatal(err)
	}

	frameLen := int(binary.LittleEndian.Uint32(out))
	stream := out[4 : 4+frameLen-4]

	dec := rawLZMA1Decode(t, stream, -1)
	bcjUnfilterX86(dec)

	if !bytes.Equal(dec, bin) {
		t.Fatal("roundtrip through compression and the inverse filter does not restore the input")
	}
}

func TestPackCRC(t *testing.T) {
	// The complement of the standard IEEE CRC-32 check value.
	if got := zbin.PackCRC([]byte("123456789")); got != 0x340bc6d9 {
		t.Fatalf("got %#x", got)
	}

	// The running CRC of data followed by its stored checksum must be zero,
	// which is the property the image decompressor checks.
	data := []byte("go-zbin checksum property")
	trailer := make([]byte, 4)
	binary.LittleEndian.PutUint32(trailer, zbin.PackCRC(data))

	if got := zbin.PackCRC(slices.Concat(data, trailer)); got != 0 {
		t.Fatalf("running checksum over data plus trailer is %#x, want 0", got)
	}
}

func TestAlignUp(t *testing.T) {
	for _, tc := range []struct {
		v    int
		a    uint32
		want int
	}{
		{0, 1, 0},
		{1, 1, 1},
		{1, 16, 16},
		{16, 16, 16},
		{17, 16, 32},
		{1012, 64, 1024},
	} {
		got, err := zbin.AlignUp(tc.v, tc.a)
		if err != nil {
			t.Fatal(err)
		}

		if got != tc.want {
			t.Fatalf("AlignUp(%d, %d) = %d, want %d", tc.v, tc.a, got, tc.want)
		}
	}

	for _, bad := range []uint32{0, 3, 24} {
		if _, err := zbin.AlignUp(1, bad); err == nil {
			t.Fatalf("AlignUp accepted alignment %d", bad)
		}
	}
}

func TestErrors(t *testing.T) {
	bin := make([]byte, 1024)

	for _, tc := range []struct {
		name  string
		match string
		zinfo []byte
	}{
		{"unknown tag", "unknown directive", records(mustRec("NOPE", 0, 0, 1))},
		{"copy overrun", "input region", records(mustRec("COPY", 1000, 100, 1))},
		{"pack overrun", "input region", records(mustRec("PACK", 0, 2000, 1))},
		{"add outside output", "beyond the output", records(mustRec("ADDB", 100, 1, 0))},
		{"add overflow", "overflows", records(mustRec("COPY", 0, 1024, 1), mustRec("ADDB", 0, 1, 0))},
		{"add underflow", "underflows", records(mustRec("COPY", 0, 16, 1), mustRec("ADDB", 0, 1, 0))},
		{"add bad divisor", "power of two", records(mustRec("COPY", 0, 16, 1), mustRec("ADDB", 0, 3, 0))},
		{"payl beyond capacity", "output overrun", records(mustRec("COPY", 0, 1, 1), mustRec("PAYL", 0, 0, 1<<17))},
		{"zrel without word size", "WithRelocWordSize", records(mustRec("ZREL", 0, 0, 243))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "add underflow" {
				bin[0] = 0xe0 // addend -32, larger than the scaled length 16
				defer func() { bin[0] = 0 }()
			}

			_, err := zbin.Compress(bin, tc.zinfo)
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error %q does not contain %q", err, tc.match)
			}
		})
	}

	if _, err := zbin.Compress(bin, make([]byte, 15)); err == nil {
		t.Fatal("expected an error for a truncated zinfo")
	}

	if _, err := zbin.Compress(bin, nil, zbin.WithRelocWordSize(6)); err == nil {
		t.Fatal("expected an error for an invalid word size")
	}
}

func TestZrelErrors(t *testing.T) {
	word := func(v uint64) []byte {
		out := make([]byte, 8)
		binary.LittleEndian.PutUint64(out, v)

		return out
	}

	relocEntry := func(offset, info, addend uint64) []byte {
		return records(word(offset), word(info), word(addend))
	}

	img := make([]byte, 64)

	for _, tc := range []struct {
		name    string
		match   string
		relocs  []byte
		machine uint32
	}{
		{"bad machine", "machine type", relocEntry(0, 3, 0), 40},
		{"unsupported type", "relocation type", relocEntry(0, 5, 0), 243},
		{"outside output", "outside the output", relocEntry(4096, 3, 0), 243},
		{"at table start", "outside the output", relocEntry(64, 3, 0), 243},
		{"word straddles table start", "outside the output", relocEntry(60, 3, 0), 243},
		{"misaligned", "not aligned", relocEntry(12, 3, 0), 243},
		{"unsorted", "not sorted", records(relocEntry(16, 3, 0), relocEntry(8, 3, 0)), 243},
		{"duplicate at zero", "not sorted", records(relocEntry(0, 3, 0), relocEntry(0, 3, 0)), 243},
		{"truncated entries", "whole number", relocEntry(0, 3, 0)[:20], 243},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := slices.Concat(img, tc.relocs)
			zinfo := records(
				mustRec("COPY", 0, uint32(len(img)), 1),
				mustRec("ZREL", uint32(len(img)), uint32(len(tc.relocs)), tc.machine),
			)

			_, err := zbin.Compress(bin, zinfo, zbin.WithRelocWordSize(8))
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error %q does not contain %q", err, tc.match)
			}
		})
	}
}

// FuzzCompress throws arbitrary directive streams and binaries at the
// public API. Success and failure are both acceptable outcomes, but a panic
// or nondeterminism is not.
func FuzzCompress(f *testing.F) {
	for _, name := range []string{"copyadd", "pack", "zrel64"} {
		bin, err := os.ReadFile(filepath.Join("testdata", name+".bin"))
		if err != nil {
			f.Fatal(err)
		}

		zinfo, err := os.ReadFile(filepath.Join("testdata", name+".zinfo"))
		if err != nil {
			f.Fatal(err)
		}

		f.Add(bin, zinfo)
	}

	f.Fuzz(func(t *testing.T, bin, zinfo []byte) {
		for _, wordSize := range []int{0, 4, 8} {
			var opts []zbin.Option
			if wordSize != 0 {
				opts = append(opts, zbin.WithRelocWordSize(wordSize))
			}

			out, err := zbin.Compress(bin, zinfo, opts...)
			again, errAgain := zbin.Compress(bin, zinfo, opts...)

			if (err == nil) != (errAgain == nil) || !bytes.Equal(out, again) {
				t.Fatalf("compression is not deterministic for word size %d", wordSize)
			}
		}
	})
}

// FuzzPackRoundtrip verifies for arbitrary payloads that the emitted frame
// decodes back to the exact input through the inverse filter, in both
// known-size and marker-terminated mode. A failure anywhere in here is an
// image that does not boot.
func FuzzPackRoundtrip(f *testing.F) {
	f.Add([]byte("hello, world"))
	f.Add(bytes.Repeat([]byte{0xe8, 0x01, 0x02, 0x03, 0x04, 0x05}, 100))
	f.Add(readFixture(f, "pack.bin"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip("payload too large for per-exec compression")
		}

		orig := bytes.Clone(data)

		out, err := zbin.Compress(data, records(mustRec("PACK", 0, uint32(len(data)), 1)))
		if err != nil {
			t.Skip("compression legitimately refused")
		}

		if !bytes.Equal(data, orig) {
			t.Fatal("Compress modified the input")
		}

		frameLen := int(binary.LittleEndian.Uint32(out))
		stream := out[4 : 4+frameLen-4]

		for _, sizeHint := range []int{len(data), -1} {
			dec := rawLZMA1Decode(t, stream, sizeHint)
			bcjUnfilterX86(dec)

			if !bytes.Equal(dec, data) {
				t.Fatalf("decode with size hint %d does not restore the input", sizeHint)
			}
		}
	})
}

// rng matches the fixture generator's deterministic generator.
type rng uint64

func (r *rng) fill(buf []byte) {
	for i := range buf {
		x := uint64(*r)
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		*r = rng(x)
		buf[i] = byte(x)
	}
}
