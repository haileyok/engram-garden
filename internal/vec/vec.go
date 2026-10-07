// Package vec holds the vector math the index is built on: half-precision
// encoding for vectors carried in records, normalization, and the two
// quantized forms the index stores (1-bit signs for scanning, int8 with a
// per-vector scale for re-ranking).
package vec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// EncodingF16LE names little-endian IEEE half precision, the only vector
// encoding records may use.
const EncodingF16LE = "f16le"

// EncodeF16 encodes v as little-endian half precision, 2 bytes per entry.
func EncodeF16(v []float32) []byte {
	out := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(out[2*i:], f32ToF16(x))
	}
	return out
}

// DecodeF16 decodes little-endian half precision. It rejects odd lengths and
// non-finite values, which no embedding model produces.
func DecodeF16(b []byte) ([]float32, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("f16 vector has an odd number of bytes")
	}
	out := make([]float32, len(b)/2)
	for i := range out {
		x := f16ToF32(binary.LittleEndian.Uint16(b[2*i:]))
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("f16 vector entry %d is not finite", i)
		}
		out[i] = x
	}
	return out, nil
}

func f16ToF32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h) & 0x3ff
	switch {
	case exp == 0:
		if frac == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal: renormalize.
		e := uint32(127 - 15 + 1)
		for frac&0x400 == 0 {
			frac <<= 1
			e--
		}
		frac &= 0x3ff
		return math.Float32frombits(sign | e<<23 | frac<<13)
	case exp == 0x1f:
		return math.Float32frombits(sign | 0xff<<23 | frac<<13)
	default:
		return math.Float32frombits(sign | (exp+127-15)<<23 | frac<<13)
	}
}

func f32ToF16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int32(b>>23&0xff) - 127 + 15
	frac := b & 0x7fffff
	switch {
	case b&0x7fffffff == 0:
		return sign
	case int32(b>>23&0xff) == 0xff: // Inf or NaN
		if frac != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	case exp >= 0x1f:
		return sign | 0x7c00
	case exp <= 0:
		if exp < -10 {
			return sign
		}
		// Subnormal, rounded to nearest even.
		m := frac | 0x800000
		shift := uint32(14 - exp)
		half := uint32(1) << (shift - 1)
		r := m >> shift
		rem := m & (1<<shift - 1)
		if rem > half || (rem == half && r&1 == 1) {
			r++
		}
		return sign | uint16(r)
	}
	r := uint32(exp)<<10 | frac>>13
	rem := frac & 0x1fff
	if rem > 0x1000 || (rem == 0x1000 && r&1 == 1) {
		r++ // may carry into the exponent, which is still correct
	}
	return sign | uint16(r)
}

// Normalize scales v to unit length in place and reports whether it could:
// a zero vector has no direction.
func Normalize(v []float32) bool {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		return false
	}
	n := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= n
	}
	return true
}

// Dot is the float32 dot product.
func Dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// BitBytes is the size of a 1-bit vector: one bit per dimension, padded to
// whole 64-bit words so scans can read words.
func BitBytes(dims int) int { return (dims + 63) / 64 * 8 }

// Int8Bytes is the size of an int8 vector: one byte per dimension plus a
// float32 scale.
func Int8Bytes(dims int) int { return dims + 4 }

// AppendBits appends v's 1-bit form: bit i is set when v[i] > 0.
func AppendBits(dst []byte, v []float32) []byte {
	words := make([]uint64, BitBytes(len(v))/8)
	for i, x := range v {
		if x > 0 {
			words[i/64] |= 1 << (i % 64)
		}
	}
	for _, w := range words {
		dst = binary.LittleEndian.AppendUint64(dst, w)
	}
	return dst
}

// Hamming is the number of differing bits between two 1-bit vectors of the
// same size.
func Hamming(a, b []byte) int {
	d := 0
	for i := 0; i+8 <= len(a); i += 8 {
		d += bits.OnesCount64(binary.LittleEndian.Uint64(a[i:]) ^ binary.LittleEndian.Uint64(b[i:]))
	}
	return d
}

// AppendInt8 appends v's int8 form: each entry scaled by 127/max|v|, then
// the scale (the multiplier back to float) as a little-endian float32.
func AppendInt8(dst []byte, v []float32) []byte {
	var m float32
	for _, x := range v {
		m = max(m, float32(math.Abs(float64(x))))
	}
	scale := float32(0)
	if m > 0 {
		scale = m / 127
	}
	for _, x := range v {
		q := int32(0)
		if scale > 0 {
			q = int32(math.Round(float64(x / scale)))
		}
		dst = append(dst, byte(int8(max(-127, min(127, q)))))
	}
	return binary.LittleEndian.AppendUint32(dst, math.Float32bits(scale))
}

// Int8Dot approximates the dot product of a full-precision query with an
// int8 vector (as written by AppendInt8).
func Int8Dot(q []float32, iv []byte) float32 {
	n := len(q)
	scale := math.Float32frombits(binary.LittleEndian.Uint32(iv[n:]))
	return int8Dot(q, iv[:n]) * scale
}

// int8DotGeneric is the reference implementation; the SIMD version must
// match it.
func int8DotGeneric(q []float32, b []byte) float32 {
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(q); i += 4 {
		s0 += q[i] * float32(int8(b[i]))
		s1 += q[i+1] * float32(int8(b[i+1]))
		s2 += q[i+2] * float32(int8(b[i+2]))
		s3 += q[i+3] * float32(int8(b[i+3]))
	}
	for ; i < len(q); i++ {
		s0 += q[i] * float32(int8(b[i]))
	}
	return s0 + s1 + s2 + s3
}
