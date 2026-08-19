package opcua

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// MaxStringLength is the maximum accepted byte length of a UA String /
// ByteString. Per OPC UA Part 6 §5.1.4 a decoder that receives a value
// longer than its limit shall truncate it (not fail); the remaining
// bytes are skipped so the stream stays aligned.
const MaxStringLength = 1 << 20 // 1 MiB

// Errors returned by the codec and the transport layer.
var (
	// ErrTooLong is returned when a decoded value would exceed the
	// decoder's fixed-size limits (e.g. array length).
	ErrTooLong = errors.New("opcua: value exceeds maximum supported length")
	// ErrInvalidEncoding is returned for structurally invalid UA
	// Binary encodings (bad encoding bytes, reserved bits, ...).
	ErrInvalidEncoding = errors.New("opcua: invalid encoding")
	// ErrUnsupportedType is returned for UA built-in types that this
	// milestone deliberately does not serialize (e.g. XmlElement,
	// DiagnosticInfo, ExpandedNodeId with URI/server index).
	ErrUnsupportedType = errors.New("opcua: unsupported UA built-in type")
)

// encoder accumulates a big-endian UA Binary byte stream.
type encoder struct{ buf []byte }

func (e *encoder) u8(v byte)     { e.buf = append(e.buf, v) }
func (e *encoder) i8(v int8)     { e.buf = append(e.buf, byte(v)) }
func (e *encoder) u16(v uint16)  { e.buf = binary.BigEndian.AppendUint16(e.buf, v) }
func (e *encoder) i16(v int16)   { e.u16(uint16(v)) }
func (e *encoder) u32(v uint32)  { e.buf = binary.BigEndian.AppendUint32(e.buf, v) }
func (e *encoder) i32(v int32)   { e.u32(uint32(v)) }
func (e *encoder) u64(v uint64)  { e.buf = binary.BigEndian.AppendUint64(e.buf, v) }
func (e *encoder) i64(v int64)   { e.u64(uint64(v)) }
func (e *encoder) f32(v float32) { e.u32(math.Float32bits(v)) }
func (e *encoder) f64(v float64) { e.u64(math.Float64bits(v)) }
func (e *encoder) bool(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}
func (e *encoder) raw(p []byte) { e.buf = append(e.buf, p...) }

// str encodes a UA String: Int32 byte length followed by UTF-8 bytes.
// The empty string is encoded with length 0 (a null string is not
// representable through this helper; see bytes for the []byte form).
func (e *encoder) str(s string) {
	e.i32(int32(len(s)))
	e.raw([]byte(s))
}

// bytes encodes a UA ByteString: Int32 length (-1 = null) + bytes.
// A nil slice encodes as null; an empty non-nil slice as length 0.
func (e *encoder) bytes(p []byte) {
	if p == nil {
		e.i32(-1)
		return
	}
	e.i32(int32(len(p)))
	e.raw(p)
}

func (e *encoder) guid(g Guid) {
	e.u32(g.Data1)
	e.u16(g.Data2)
	e.u16(g.Data3)
	e.raw(g.Data4[:])
}

// decoder reads a big-endian UA Binary byte stream with bounds checks.
// Every read returns io.ErrUnexpectedEOF when the stream is exhausted.
type decoder struct {
	b   []byte
	off int
}

func (d *decoder) need(n int) error {
	if n < 0 || d.off+n > len(d.b) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (d *decoder) skip(n int) error {
	if err := d.need(n); err != nil {
		return err
	}
	d.off += n
	return nil
}

func (d *decoder) u8() (byte, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	v := d.b[d.off]
	d.off++
	return v, nil
}

func (d *decoder) i8() (int8, error) {
	v, err := d.u8()
	return int8(v), err
}

func (d *decoder) u16() (uint16, error) {
	if err := d.need(2); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint16(d.b[d.off:])
	d.off += 2
	return v, nil
}

func (d *decoder) i16() (int16, error) {
	v, err := d.u16()
	return int16(v), err
}

func (d *decoder) u32() (uint32, error) {
	if err := d.need(4); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(d.b[d.off:])
	d.off += 4
	return v, nil
}

func (d *decoder) i32() (int32, error) {
	v, err := d.u32()
	return int32(v), err
}

func (d *decoder) u64() (uint64, error) {
	if err := d.need(8); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint64(d.b[d.off:])
	d.off += 8
	return v, nil
}

func (d *decoder) i64() (int64, error) {
	v, err := d.u64()
	return int64(v), err
}

func (d *decoder) f32() (float32, error) {
	v, err := d.u32()
	return math.Float32frombits(v), err
}

func (d *decoder) f64() (float64, error) {
	v, err := d.u64()
	return math.Float64frombits(v), err
}

func (d *decoder) bool() (bool, error) {
	v, err := d.u8()
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// str decodes a UA String. A null string (length -1) decodes to "".
// Any other negative length is invalid (OPC UA Part 6 §5.2.2.4).
// A string longer than MaxStringLength is truncated to
// MaxStringLength bytes; the remaining bytes are skipped so the stream
// stays aligned (OPC UA Part 6 §5.1.4).
func (d *decoder) str() (string, error) {
	n, err := d.i32()
	if err != nil {
		return "", err
	}
	if n == -1 {
		return "", nil
	}
	if n < 0 {
		return "", fmt.Errorf("%w: negative String length %d", ErrInvalidEncoding, n)
	}
	if err := d.need(int(n)); err != nil {
		return "", err
	}
	ln := int(n)
	if ln > MaxStringLength {
		ln = MaxStringLength
	}
	s := string(d.b[d.off : d.off+ln])
	d.off += int(n)
	return s, nil
}

// bytes decodes a UA ByteString. A null byte string (length -1)
// decodes to nil. Any other negative length is invalid. Over-long
// values follow the same truncation rule as str.
func (d *decoder) bytes() ([]byte, error) {
	n, err := d.i32()
	if err != nil {
		return nil, err
	}
	if n == -1 {
		return nil, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("%w: negative ByteString length %d", ErrInvalidEncoding, n)
	}
	if err := d.need(int(n)); err != nil {
		return nil, err
	}
	ln := int(n)
	if ln > MaxStringLength {
		ln = MaxStringLength
	}
	out := make([]byte, ln)
	copy(out, d.b[d.off:d.off+ln])
	d.off += int(n)
	return out, nil
}

func (d *decoder) guid() (Guid, error) {
	var g Guid
	var err error
	if g.Data1, err = d.u32(); err != nil {
		return g, err
	}
	if g.Data2, err = d.u16(); err != nil {
		return g, err
	}
	if g.Data3, err = d.u16(); err != nil {
		return g, err
	}
	if err := d.need(8); err != nil {
		return g, err
	}
	copy(g.Data4[:], d.b[d.off:d.off+8])
	d.off += 8
	return g, nil
}
