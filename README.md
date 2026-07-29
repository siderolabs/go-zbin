# go-zbin

Pure Go implementation of the compressed image format used by the iPXE build system, replacing the `zbin` host tool for the compression side.

iPXE builds BIOS images by splitting the linked binary into regions that must stay uncompressed (boot sector, PXE header) and regions that get LZMA compressed, then patching final sizes into the uncompressed header.
The recipe for this is emitted by the build itself as a stream of 16-byte directives ("zinfo").
This module interprets that directive stream and produces images that the decompressor embedded in iPXE images accepts.

The typical use case is patching an iPXE binary, for example swapping the embedded script in an already built image, and recompressing it without shelling out to the C tool or carrying it and its liblzma dependency around.

The format is documented in [FORMAT.md](FORMAT.md).
This is an independent implementation of that format written for interoperability.
It shares no code with the iPXE project and is not affiliated with it.

## Usage

```go
import "github.com/siderolabs/go-zbin/zbin"

out, err := zbin.Compress(bin, zinfo)
```

`bin` is the flat binary and `zinfo` the directive stream, both as produced by the iPXE build (`objcopy -R .zinfo` and `objcopy -j .zinfo` of the intermediate object).
Images that carry ZREL relocation directives additionally need the target's machine word size, `zbin.WithRelocWordSize(4)` or `zbin.WithRelocWordSize(8)`, matching the 32-bit and 64-bit variants of the original tool.
Images without ZREL directives need no option.

## Compression ratio

LZMA encoding uses [github.com/ulikunitz/xz](https://github.com/ulikunitz/xz) with the fixed stream properties the format requires.
Its encoder is not as strong as liblzma's: on a real `undionly.kpxe` build the resulting image is about 7% larger than the C tool's output.
The streams themselves are fully compatible.

## Testing

`go test ./...` runs against committed fixtures whose expected outputs were generated with the original C tool.
Fixtures without a compression step must match byte for byte.
Compressed fixtures are verified structurally, including decoding both this module's and the reference streams back to the identical filtered input.

To regenerate fixtures or run the live comparison against a real build, see `zbin/testdata/gen` and the environment variables in `oracle_live_test.go`.
