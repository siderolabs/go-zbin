# The zbin image format

This document describes the compressed image format used by the iPXE build system, as implemented by this module.
It is a description of the wire format: the record layouts, framing, fixed parameters and transformations that any interoperable implementation must produce.
The implementation in this module is written against this description.

## Inputs

An image is built from two inputs.

- The **flat binary**: the linked program as a raw byte stream.
- The **directive stream** ("zinfo"): a sequence of fixed-size records extracted from a dedicated section of the linked program.
  It instructs the builder how to assemble the output from pieces of the flat binary.

## Directive records

The directive stream is a flat array of 16-byte records.
Each record starts with a 4-character ASCII tag followed by three little-endian 32-bit fields.
One record type (`BASE`) instead carries a 64-bit field in the last 8 bytes.

```text
offset  size  content
0       4     ASCII tag
4       4     field A (u32 LE)
8       4     field B (u32 LE)
12      4     field C (u32 LE)
```

Records are processed strictly in order.
Three pieces of state persist across records:

- the **output position**, the number of bytes emitted so far,
- the **header mark**, the output position recorded by the `PAYL` record,
- the **base address**, recorded by the `BASE` record.

Alignment always rounds the output position up to the next multiple of the given power of two.
Bytes skipped by alignment hold the value `0xff`.

## Directive types

### COPY: A=input offset, B=length, C=alignment

Align the output position, then append `length` bytes verbatim from the flat binary at `input offset`.

### PACK: A=input offset, B=length, C=alignment

Align the output position, then append the given input region in compressed, framed form:

```text
u32 LE   frame length: compressed stream length + 4
bytes    raw LZMA stream
u32 LE   checksum over the raw LZMA stream bytes
```

Before compression, the region is transformed in place by the branch conversion filter described below.
The transformation is visible to any later directive that reads the same input bytes.

The compressed stream is a headerless (raw) LZMA1 stream with the fixed properties **lc=2, lp=0, pb=0**, terminated by an **end-of-payload marker**.
The decompressor embedded in the image consumes the stream until it finds the marker, so the marker is mandatory.
The dictionary size is not encoded and effectively unbounded, so the encoder may use any dictionary at least as large as the region.

The checksum is CRC-32 with the reflected polynomial `0xedb88320`, seeded with all ones and stored **without** the final bit inversion.
Storing it in this form makes the running CRC of the stream bytes followed by the checksum field itself come out as zero, which is how the decompressor verifies it.
It equals the bitwise complement of the standard IEEE CRC-32.

### Branch conversion filter (part of PACK)

x86 near `call` (opcode `0xe8`) and near `jmp` (opcode `0xe9`) instructions carry a 32-bit little-endian displacement relative to the end of the instruction.
Rewriting these to absolute block offsets makes repeated references to one target byte-identical and thus more compressible.
The decompressor applies the exact inverse after decompressing, so the transform must be reproduced bit for bit.

Scan the block from offset 0 with `limit = block length - 5`.
At each offset holding the byte `0xe8` or `0xe9`, take the signed 32-bit value `target` at the next 4 bytes and with `off` being the opcode offset:

- if `target` is in `[-off, limit-off)`, the branch resolves inside the block: replace it with the absolute offset `target + off`, a value in `[0, limit)`,
- else if `target` is in `[limit-off, limit)`, it is not rewritten as absolute, but it collides with the absolute values produced by the rule above: replace it with `target - limit`, a value in `[-off, 0)` that no untouched displacement can have,
- otherwise leave it unchanged.

Then continue scanning after the 4 displacement bytes, so a displacement is never itself inspected for opcode bytes.
Blocks shorter than 5 bytes are left untouched.

The two rules together keep the mapping bijective given only the block length, which is all the decompressor has.

### PAYL: C=alignment

Align the output position and record it as the header mark.
Nothing is emitted.
The mark splits the image into the uncompressed header, typically executed directly by the boot firmware, and the payload that follows.

### ADD family: A=output offset, B=divisor

Twelve tags patch a little-endian integer field previously emitted into the output, so that the header can carry sizes that are only known once compression is done.
The fourth tag character selects the field width: `B` is 1 byte, `W` is 2 bytes, `L` is 4 bytes.

| Tags             | Length taken            | Offset relative to |
|------------------|-------------------------|--------------------|
| ADDB, ADDW, ADDL | total output length     | start of output    |
| ADHB, ADHW, ADHL | header mark             | start of output    |
| ADPB, ADPW, ADPL | total minus header mark | start of output    |
| APPB, APPW, APPL | total minus header mark | header mark        |

The length is divided by the divisor, rounding up.
The divisor must be a power of two: the reference tool computes the scaled length with the same bitmask alignment it uses for output positions, which equals ceiling division only for powers of two, so other divisors have no well-defined meaning and this module rejects them.
The field's current content, read as a **signed** value of the field width, is added, and the sum is stored back.
A negative sum or a sum exceeding the field width is an error.

### BASE: bytes 8-16 = link base address (u64 LE)

Record the link-time base address for later `ZREL` records.
Nothing is emitted.

### ZREL: A=input offset, B=length, C=ELF machine type

Read ELF `Rela` dynamic relocation entries from the given input region, apply the relative relocations to the already emitted output and append a compressed relocation table.
The output position is first aligned to the machine word size.

The machine word size (4 or 8 bytes) is a build-level choice matching the target, not encoded in the directive.
It determines both the `Rela` entry layout (three little-endian machine words: offset, info, addend) and the table entry size.
Because a wrong word size can parse a relocation region into plausible garbage without any detectable error, this module requires the caller to state it explicitly and requires the region length to be a whole number of entries, where the reference tool silently ignores trailing bytes.
The only machine type defined so far is RISC-V (243), whose relocation entries have a 3-word `Rela` layout.
Type 0 entries (`R_RISCV_NONE`) are skipped.
Type 3 entries (`R_RISCV_RELATIVE`) have their addend written to the output at `r_offset - base address`, which must be word-aligned, inside the emitted output and in strictly increasing order.
Any other type is an error.

The table encodes which machine words of the loaded image need the load offset added, as a delta-compressed bitmap based on the ELF `Relr` idea.
Each table entry is one little-endian machine word, in one of two shapes distinguished by the most significant bit N:

```text
N=1:  1 RRR...RRR                     bitmap of the next (bits-1) words
N=0:  0 SSS...SSS (19 bits) RRR...R   skip count, then bitmap of (bits-20) words
```

Each entry covers a window of machine words following the current window position.
An R bit set at position i (counting from the least significant bit) marks word i of the window as needing relocation.
An N=0 entry first advances the window position by the 19-bit skip count taken from the bits directly below the top bit.
After any entry, the window advances by the number of R bit positions the entry has.
An N=0 entry whose skip count is zero terminates the table after its bitmap is applied.

The canonical encoding this module produces, matching the original tool: entries are opened lazily as relocations arrive.
When the next relocation's word delta from the current window start fits the open entry's bitmap, its bit is set.
Otherwise a new entry is opened after advancing the window past the previous bitmap: a gap shorter than 19 words (cheaper as zero bits than as a skip count) opens an N=1 entry, a gap of up to 2^19-1 words opens an N=0 entry consuming the whole gap as the skip count, and a larger gap opens an N=0 entry with the maximum skip count and continues from there.
At the end, if the last entry has a zero skip count it becomes the terminator by clearing its N bit, otherwise a fresh all-zero entry is appended.
An empty relocation set produces a single all-zero entry.

## Output sizing

The original tool sizes its output buffer at four times the flat binary length and fails if the assembled image exceeds it.
This module keeps that sizing as a sanity bound with a small fixed minimum for tiny inputs.
