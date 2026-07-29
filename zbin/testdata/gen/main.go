// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Fixture generator. It builds synthetic flat binaries and directive
// streams, runs the original C zbin tool on them and stores its output as
// the expected ("golden") results for the tests.
//
// It requires the paths of the 32-bit and 64-bit oracle builds:
//
//	cd zbin && ZBIN32=/path/to/zbin32 ZBIN64=/path/to/zbin64 go run ./testdata/gen
//
// The oracle binaries are built from the iPXE source tree, which is not
// part of this repository. The generated inputs and outputs are committed,
// so the tests do not need the oracle.
package main

import (
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	elfMachineRISCV = 243

	relocNone     = 0
	relocRelative = 3

	skipMax = 1<<19 - 1
)

var outDir string

func main() {
	zbin32 := os.Getenv("ZBIN32")
	zbin64 := os.Getenv("ZBIN64")

	if zbin32 == "" || zbin64 == "" {
		fmt.Fprintln(os.Stderr, "ZBIN32 and ZBIN64 must point at oracle zbin builds")
		os.Exit(1)
	}

	outDir = "testdata"
	if _, err := os.Stat(outDir); err != nil {
		fmt.Fprintln(os.Stderr, "run from the zbin package directory")
		os.Exit(1)
	}

	genCopyAdd(zbin64)
	genPack(zbin64)

	// The ZREL fixtures pin every branch of the table encoder against the
	// oracle: bitmap capacity edges, the short-gap versus skip-count tie at
	// 19 words, skip chaining past the 19-bit maximum, both terminator
	// forms and the empty set.
	genZrel(zrelFixture{name: "zrel32", oracle: zbin32, wordSize: 4, imgWords: 1024, targets: []int{0, 1, 4, 42, 70, 200, 210}})
	genZrel(zrelFixture{name: "zrel64", oracle: zbin64, wordSize: 8, imgWords: 1024, targets: []int{0, 1, 4, 42, 70, 200, 210}})
	genZrel(zrelFixture{name: "zrelbig32", oracle: zbin32, wordSize: 4, imgWords: 2*skipMax + 40, targets: []int{0, 2 * skipMax}, gz: true})
	genZrel(zrelFixture{name: "zreldense64", oracle: zbin64, wordSize: 8, imgWords: 64, targets: []int{0, 1, 4}})
	genZrel(zrelFixture{name: "zreltie32", oracle: zbin32, wordSize: 4, imgWords: 128, targets: []int{0, 50}})
	genZrel(zrelFixture{name: "zrelgap32", oracle: zbin32, wordSize: 4, imgWords: 128, targets: []int{0, 49}})
	genZrel(zrelFixture{name: "zreledge32", oracle: zbin32, wordSize: 4, imgWords: 128, targets: []int{0, 30, 31}})
	genZrel(zrelFixture{name: "zrelempty64", oracle: zbin64, wordSize: 8, imgWords: 64, targets: nil})

	fmt.Println("fixtures written to", outDir)
}

// rng is a fixed-seed xorshift generator so the fixtures are reproducible.
type rng uint64

func (r *rng) next() uint64 {
	x := uint64(*r)
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	*r = rng(x)

	return x
}

func (r *rng) fill(buf []byte) {
	for i := range buf {
		buf[i] = byte(r.next())
	}
}

// fillCode fills buf with compressible pseudo-code: repeated short motifs
// with pseudo-random branch instructions mixed in.
func (r *rng) fillCode(buf []byte) {
	motif := make([]byte, 24)
	r.fill(motif)

	for i := 0; i < len(buf); i += len(motif) {
		copy(buf[i:], motif)
	}

	for i := 0; i+5 < len(buf); i += 32 + int(r.next()%64) {
		op := byte(0xe8)
		if r.next()%2 == 0 {
			op = 0xe9
		}

		buf[i] = op
		binary.LittleEndian.PutUint32(buf[i+1:], uint32(r.next()))
	}
}

func rec(tag string, a, b, c uint32) []byte {
	out := make([]byte, 16)
	copy(out, tag)
	binary.LittleEndian.PutUint32(out[4:], a)
	binary.LittleEndian.PutUint32(out[8:], b)
	binary.LittleEndian.PutUint32(out[12:], c)

	return out
}

func recBase(base uint64) []byte {
	out := make([]byte, 16)
	copy(out, "BASE")
	binary.LittleEndian.PutUint64(out[8:], base)

	return out
}

func runOracle(oracle string, bin, zinfo []byte) []byte {
	dir, err := os.MkdirTemp("", "zbin-gen")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	binPath := filepath.Join(dir, "in.bin")
	zinfoPath := filepath.Join(dir, "in.zinfo")

	if err := os.WriteFile(binPath, bin, 0o644); err != nil {
		panic(err)
	}

	if err := os.WriteFile(zinfoPath, zinfo, 0o644); err != nil {
		panic(err)
	}

	cmd := exec.Command(oracle, binPath, zinfoPath)
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		panic(fmt.Sprintf("oracle failed: %v", err))
	}

	return out
}

func write(name string, data []byte, compress bool) {
	path := filepath.Join(outDir, name)

	if compress {
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		defer f.Close()

		w := gzip.NewWriter(f)
		if _, err := w.Write(data); err != nil {
			panic(err)
		}

		if err := w.Close(); err != nil {
			panic(err)
		}

		return
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}

// genCopyAdd exercises COPY, PAYL and all twelve ADD directives. The script
// contains no PACK, so the oracle output is fully deterministic and the
// test compares byte for byte.
func genCopyAdd(oracle string) {
	r := rng(0x1234567890abcdef)
	bin := make([]byte, 4096)
	r.fill(bin)

	// Seed the patch fields with known addends, some of them negative.
	bin[0x100] = 0x05                                          // ADDB
	bin[0x102] = 0xfd                                          // ADHB, -3
	binary.LittleEndian.PutUint16(bin[0x104:], 0x0010)         // ADDW
	binary.LittleEndian.PutUint32(bin[0x108:], 0x00000100)     // ADDL
	bin[0x10c] = 0x00                                          // ADPB
	binary.LittleEndian.PutUint16(bin[0x10e:], 0xfffe)         // ADHW, -2
	binary.LittleEndian.PutUint32(bin[0x110:], 0xfffffff0)     // ADHL, -16
	binary.LittleEndian.PutUint16(bin[0x114:], 0x0007)         // ADPW
	binary.LittleEndian.PutUint32(bin[0x118:], 0x00000009)     // ADPL
	bin[2048+0x10] = 0x02                                      // APPB
	binary.LittleEndian.PutUint16(bin[2048+0x12:], 0x0001)     // APPW
	binary.LittleEndian.PutUint32(bin[2048+0x14:], 0x00000003) // APPL

	var zinfo []byte
	for _, r := range [][]byte{
		rec("COPY", 0, 512, 16),
		rec("COPY", 1024, 500, 32),
		rec("PAYL", 0, 0, 64),
		rec("COPY", 2048, 1000, 4),
		rec("ADDB", 0x100, 16, 0),
		rec("ADDW", 0x104, 1, 0),
		rec("ADDL", 0x108, 1, 0),
		rec("ADHB", 0x102, 8, 0),
		rec("ADHW", 0x10e, 1, 0),
		rec("ADHL", 0x110, 1, 0),
		rec("ADPB", 0x10c, 16, 0),
		rec("ADPW", 0x114, 2, 0),
		rec("ADPL", 0x118, 1, 0),
		rec("APPB", 0x10, 16, 0),
		rec("APPW", 0x12, 4, 0),
		rec("APPL", 0x14, 1, 0),
	} {
		zinfo = append(zinfo, r...)
	}

	write("copyadd.bin", bin, false)
	write("copyadd.zinfo", zinfo, false)
	write("copyadd.golden", runOracle(oracle, bin, zinfo), false)
}

// genPack exercises PACK, including branch filter edge cases, plus PAYL and
// a size patch. A COPY overlapping the first packed region pins the filter's
// statefulness: the copied bytes must be the filtered ones. Compressed
// output differs between encoders, so the test compares this fixture
// structurally instead of byte for byte.
func genPack(oracle string) {
	r := rng(0xfedcba9876543210)
	bin := make([]byte, 8192)
	r.fill(bin)

	// Prefix with a zeroed size field patched by ADDW.
	binary.LittleEndian.PutUint16(bin[0x40:], 0)

	// Code-like region for the first PACK.
	code := bin[512 : 512+3000]
	r.fillCode(code)

	// Branch filter edge cases at fixed positions inside the code region.
	// The region length is 3000, so limit = 2995.
	limit := int32(2995)
	putJump := func(off int, opcode byte, target int32) {
		code[off] = opcode
		binary.LittleEndian.PutUint32(code[off+1:], uint32(target))
	}

	putJump(100, 0xe8, -100)       // lowest in-range target, becomes 0
	putJump(200, 0xe8, limit-201)  // highest in-range target
	putJump(300, 0xe9, limit-300)  // first colliding value, becomes -300
	putJump(400, 0xe8, limit-1)    // last colliding value, becomes -1
	putJump(500, 0xe8, limit)      // just above, stays
	putJump(600, 0xe9, -601)       // just below, stays
	putJump(700, 0xe8, 1<<30)      // far positive, stays
	putJump(800, 0xe9, -(1 << 30)) // far negative, stays
	putJump(900, 0xe8, 0x00e8e8e8) // opcode bytes inside a displacement stay unscanned

	// Second PACK region, partly repetitive.
	r.fillCode(bin[4096 : 4096+4096])

	var zinfo []byte
	for _, r := range [][]byte{
		rec("COPY", 0, 256, 16),
		rec("PACK", 512, 3000, 16),
		rec("COPY", 512, 100, 16),
		rec("PAYL", 0, 0, 512),
		rec("PACK", 4096, 4096, 16),
		rec("ADDW", 0x40, 16, 0),
	} {
		zinfo = append(zinfo, r...)
	}

	write("pack.bin", bin, false)
	write("pack.zinfo", zinfo, false)
	write("pack.golden", runOracle(oracle, bin, zinfo), false)
}

// zrelFixture describes one BASE plus COPY plus ZREL fixture. The targets
// are machine word indexes into the image that receive relative
// relocations. An empty target list produces a single no-op relocation
// entry, pinning the empty-table terminator. The output is deterministic,
// so the tests compare byte for byte.
type zrelFixture struct {
	name     string
	oracle   string
	wordSize int
	imgWords int
	targets  []int
	gz       bool
}

func genZrel(f zrelFixture) {
	const base = 0x80000000

	r := rng(0x0f1e2d3c4b5a6978)
	img := make([]byte, f.imgWords*f.wordSize)

	if f.gz {
		// Keep the gzipped fixture small: only the head is random.
		r.fill(img[:4096])
	} else {
		r.fill(img)
	}

	putWord := func(buf []byte, v uint64) {
		if f.wordSize == 4 {
			binary.LittleEndian.PutUint32(buf, uint32(v))
		} else {
			binary.LittleEndian.PutUint64(buf, v)
		}
	}

	var relocs []byte

	addReloc := func(offset uint64, info, addend uint64) {
		entry := make([]byte, 3*f.wordSize)
		putWord(entry, offset)
		putWord(entry[f.wordSize:], info)
		putWord(entry[2*f.wordSize:], addend)
		relocs = append(relocs, entry...)
	}

	if len(f.targets) == 0 {
		addReloc(base, relocNone, 0)
	}

	for i, w := range f.targets {
		addReloc(base+uint64(w*f.wordSize), relocRelative, 0x1111111111111111*uint64(i+1))

		if i == 1 {
			// A no-op entry in the middle must be skipped.
			addReloc(base, relocNone, 0)
		}
	}

	bin := append(append([]byte(nil), img...), relocs...)

	var zinfo []byte
	for _, r := range [][]byte{
		recBase(base),
		rec("COPY", 0, uint32(len(img)), 16),
		rec("ZREL", uint32(len(img)), uint32(len(relocs)), elfMachineRISCV),
	} {
		zinfo = append(zinfo, r...)
	}

	write(f.name+".bin"+gzSuffix(f.gz), bin, f.gz)
	write(f.name+".zinfo", zinfo, false)
	write(f.name+".golden"+gzSuffix(f.gz), runOracle(f.oracle, bin, zinfo), f.gz)
}

func gzSuffix(gz bool) string {
	if gz {
		return ".gz"
	}

	return ""
}
