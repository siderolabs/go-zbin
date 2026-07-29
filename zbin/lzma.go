// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package zbin

import (
	"bytes"
	"fmt"
	"hash/crc32"

	"github.com/ulikunitz/xz/lzma"
)

// The format uses headerless (raw) LZMA streams with fixed encoding
// properties, matched to the decompressor embedded in the images. The
// decompressor consumes the stream until it finds the end-of-payload marker,
// so the streams carry no header but do carry the marker.
const (
	lzmaLC = 2
	lzmaLP = 0
	lzmaPB = 0

	// lzmaHeaderLen is the size of the classic LZMA container header that
	// the encoder emits and that is stripped to obtain the raw stream.
	lzmaHeaderLen = 13

	// lzmaMinDictCap is the smallest dictionary capacity the encoder accepts.
	lzmaMinDictCap = 4096
)

// compressLZMA encodes data as a raw LZMA stream with the fixed properties
// of the format, terminated by the end-of-payload marker. The marker is
// mandatory: the decompressor embedded in the images decodes until it finds
// the marker, so a stream without one never terminates decompression.
func compressLZMA(data []byte) ([]byte, error) {
	dictCap := max(len(data), lzmaMinDictCap)

	// HashTable4 outperforms the BinaryTree matcher considerably on machine
	// code, staying within a few percent of the liblzma reference encoder.
	cfg := lzma.WriterConfig{
		Properties: &lzma.Properties{LC: lzmaLC, LP: lzmaLP, PB: lzmaPB},
		DictCap:    dictCap,
		EOSMarker:  true,
		Matcher:    lzma.HashTable4,
	}

	var buf bytes.Buffer

	w, err := cfg.NewWriter(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to create LZMA writer: %w", err)
	}

	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("failed to compress: %w", err)
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("failed to finish compression: %w", err)
	}

	if buf.Len() < lzmaHeaderLen {
		return nil, fmt.Errorf("encoder produced %d bytes, less than the %d-byte container header", buf.Len(), lzmaHeaderLen)
	}

	return buf.Bytes()[lzmaHeaderLen:], nil
}

// packCRC computes the checksum trailer of a compressed region: CRC-32 with
// the reflected polynomial 0xedb88320, seeded with all ones and taken
// without the final inversion. It equals the bitwise complement of the
// standard IEEE CRC-32.
func packCRC(data []byte) uint32 {
	return ^crc32.ChecksumIEEE(data)
}
