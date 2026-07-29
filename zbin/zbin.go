// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package zbin builds compressed iPXE images from a flat binary and its
// compression directives ("zinfo"), producing output interoperable with
// images built by the iPXE build system.
//
// The implementation follows the format description in FORMAT.md. It is an
// independent implementation written for interoperability and shares no code
// with the iPXE project.
package zbin

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// recordLen is the fixed size of a single zinfo directive record.
const recordLen = 16

// minOutputCap is the minimum capacity of the output buffer. The output
// buffer is otherwise sized at four times the input, which mirrors the sizing
// of the original tool but would be too tight for very small inputs, where
// the fixed compression framing can exceed the input size.
const minOutputCap = 64 * 1024

type options struct {
	relocWordSize int
}

// Option customizes the compression.
type Option func(*options)

// WithRelocWordSize sets the machine word size in bytes (4 or 8) used both
// for the compressed relocation records emitted for ZREL directives and for
// parsing the ELF relocation entries they refer to. It corresponds to the
// 32-bit and 64-bit build variants of the original tool.
//
// The word size is not encoded in the directive stream and a wrong value can
// produce a silently broken image, so there is no default: compressing an
// image whose zinfo contains ZREL directives fails unless this option is
// given. Images without ZREL directives need no option.
func WithRelocWordSize(size int) Option {
	return func(o *options) {
		o.relocWordSize = size
	}
}

// Compress interprets the zinfo directives against the flat binary bin and
// returns the resulting compressed image. The bin slice is not modified.
func Compress(bin, zinfo []byte, opts ...Option) ([]byte, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	if o.relocWordSize != 0 && o.relocWordSize != 4 && o.relocWordSize != 8 {
		return nil, fmt.Errorf("invalid relocation word size %d: must be 4 or 8", o.relocWordSize)
	}

	if len(zinfo)%recordLen != 0 {
		return nil, fmt.Errorf("zinfo length %d is not a multiple of the %d-byte record size", len(zinfo), recordLen)
	}

	c := &compressor{
		// The PACK directive transforms its input region in place before
		// compressing it, so work on a private copy of the input.
		in: append([]byte(nil), bin...),
		// Gaps introduced by alignment hold 0xff, not zero.
		out:           bytes.Repeat([]byte{0xff}, max(4*len(bin), minOutputCap)),
		relocWordSize: o.relocWordSize,
	}

	for off := 0; off < len(zinfo); off += recordLen {
		rec := zinfo[off : off+recordLen]
		if err := c.process(rec); err != nil {
			return nil, fmt.Errorf("zinfo record %d (%q): %w", off/recordLen, string(rec[:4]), err)
		}
	}

	// Return an exact-size copy so callers do not pin the working buffer.
	return bytes.Clone(c.out[:c.len]), nil
}

// compressor holds the state threaded through the directive stream.
type compressor struct {
	in  []byte
	out []byte

	// len is the current output length, advanced by every directive that
	// emits or aligns data.
	len int

	// hdrLen is the output length recorded at the PAYL marker, splitting the
	// image into an uncompressed header and the payload that follows.
	hdrLen int

	// base is the link-time base address recorded by a BASE directive,
	// subtracted from ELF relocation offsets by ZREL.
	base uint64

	relocWordSize int
}

// ensure fails unless the output can hold n bytes. Every advance of the
// output position goes through this single check.
func (c *compressor) ensure(n int) error {
	if n > len(c.out) {
		return fmt.Errorf("output overrun: position %#x exceeds the output capacity %#x", n, len(c.out))
	}

	return nil
}

// lengthSource selects which length an ADD-family directive patches in.
type lengthSource int

const (
	lengthTotal   lengthSource = iota // current total output length
	lengthHeader                      // header length as marked by PAYL
	lengthPayload                     // total length minus the header length
)

// addDirectives enumerates the ADD-family directive tags. The fourth tag
// letter selects the field width. The APP variants both use the payload
// length and take the patch offset relative to the end of the header.
var addDirectives = map[string]struct {
	width      int
	source     lengthSource
	fromHeader bool
}{
	"ADDB": {1, lengthTotal, false},
	"ADDW": {2, lengthTotal, false},
	"ADDL": {4, lengthTotal, false},
	"ADHB": {1, lengthHeader, false},
	"ADHW": {2, lengthHeader, false},
	"ADHL": {4, lengthHeader, false},
	"ADPB": {1, lengthPayload, false},
	"ADPW": {2, lengthPayload, false},
	"ADPL": {4, lengthPayload, false},
	"APPB": {1, lengthPayload, true},
	"APPW": {2, lengthPayload, true},
	"APPL": {4, lengthPayload, true},
}

func (c *compressor) process(rec []byte) error {
	tag := string(rec[0:4])
	a := binary.LittleEndian.Uint32(rec[4:8])
	b := binary.LittleEndian.Uint32(rec[8:12])
	d := binary.LittleEndian.Uint32(rec[12:16])

	switch tag {
	case "COPY":
		return c.copyRegion(a, b, d)
	case "PACK":
		return c.pack(a, b, d)
	case "PAYL":
		n, err := alignUp(c.len, d)
		if err != nil {
			return err
		}

		if err := c.ensure(n); err != nil {
			return err
		}

		c.len = n
		c.hdrLen = n

		return nil
	case "BASE":
		c.base = binary.LittleEndian.Uint64(rec[8:16])

		return nil
	case "ZREL":
		return c.zrel(a, b, d)
	}

	if spec, ok := addDirectives[tag]; ok {
		var length int

		switch spec.source {
		case lengthTotal:
			length = c.len
		case lengthHeader:
			length = c.hdrLen
		case lengthPayload:
			length = c.len - c.hdrLen
		}

		target := int64(a)
		if spec.fromHeader {
			target += int64(c.hdrLen)
		}

		return c.add(target, spec.width, length, b)
	}

	return errors.New("unknown directive")
}

// copyRegion appends a verbatim copy of an input region to the output,
// aligning the output position first.
func (c *compressor) copyRegion(offset, length, alignment uint32) error {
	src, err := c.inputRegion(offset, length)
	if err != nil {
		return err
	}

	n, err := alignUp(c.len, alignment)
	if err != nil {
		return err
	}

	if err := c.ensure(n + len(src)); err != nil {
		return err
	}

	copy(c.out[n:], src)
	c.len = n + len(src)

	return nil
}

// add patches a little-endian integer field of the given width inside the
// already emitted output. The field's current value acts as a signed addend,
// to which the length divided by the divisor (rounded up) is added.
func (c *compressor) add(target int64, width, length int, divisor uint32) error {
	if target < 0 || target+int64(width) > int64(c.len) {
		return fmt.Errorf("patch target [%#x,%#x) is beyond the output length %#x", target, target+int64(width), c.len)
	}

	// The reference tool scales with the same bitmask alignment it uses for
	// output positions, which equals ceiling division only for powers of
	// two, so other divisors have no well-defined meaning.
	if divisor == 0 || divisor&(divisor-1) != 0 {
		return fmt.Errorf("divisor %d is not a power of two", divisor)
	}

	offset := int(target)
	scaled := (length + int(divisor) - 1) / int(divisor)

	var addend int64

	switch width {
	case 1:
		addend = int64(int8(c.out[offset]))
	case 2:
		addend = int64(int16(binary.LittleEndian.Uint16(c.out[offset:])))
	case 4:
		addend = int64(int32(binary.LittleEndian.Uint32(c.out[offset:])))
	}

	val := int64(scaled) + addend
	if val < 0 {
		return fmt.Errorf("patched value %d at %#x underflows the field", val, offset)
	}

	mask := int64(1)<<(8*width) - 1
	if val&^mask != 0 {
		return fmt.Errorf("patched value %#x at %#x overflows the %d-byte field", val, offset, width)
	}

	switch width {
	case 1:
		c.out[offset] = byte(val)
	case 2:
		binary.LittleEndian.PutUint16(c.out[offset:], uint16(val))
	case 4:
		binary.LittleEndian.PutUint32(c.out[offset:], uint32(val))
	}

	return nil
}

// pack compresses an input region and appends it to the output, framed as a
// little-endian u32 length, the raw LZMA stream, and a u32 checksum. The
// length field counts the stream and the checksum. The input region is BCJ
// filtered in place before compression.
func (c *compressor) pack(offset, length, alignment uint32) error {
	region, err := c.inputRegion(offset, length)
	if err != nil {
		return err
	}

	n, err := alignUp(c.len, alignment)
	if err != nil {
		return err
	}

	lenField := n

	if err = c.ensure(lenField + 4); err != nil {
		return err
	}

	c.len = lenField + 4

	bcjFilterX86(region)

	packed, err := compressLZMA(region)
	if err != nil {
		return err
	}

	if err = c.ensure(c.len + len(packed) + 4); err != nil {
		return err
	}

	copy(c.out[c.len:], packed)
	c.len += len(packed)

	binary.LittleEndian.PutUint32(c.out[c.len:], packCRC(packed))
	c.len += 4

	binary.LittleEndian.PutUint32(c.out[lenField:], uint32(len(packed)+4))

	return nil
}

// inputRegion bounds-checks a directive's input range, with the arithmetic
// in uint64 so 32-bit wire fields cannot wrap on 32-bit hosts.
func (c *compressor) inputRegion(offset, length uint32) ([]byte, error) {
	end := uint64(offset) + uint64(length)
	if end > uint64(len(c.in)) {
		return nil, fmt.Errorf("input region [%#x,%#x) is beyond the input length %#x", offset, end, len(c.in))
	}

	return c.in[offset:end:end], nil
}

func alignUp(v int, alignment uint32) (int, error) {
	if alignment == 0 || alignment&(alignment-1) != 0 {
		return 0, fmt.Errorf("alignment %d is not a power of two", alignment)
	}

	aligned := (uint64(v) + uint64(alignment) - 1) &^ (uint64(alignment) - 1)
	if aligned > math.MaxInt {
		return 0, fmt.Errorf("aligned position %#x overflows", aligned)
	}

	return int(aligned), nil
}
