//go:build goexperiment.simd && amd64

// The int8 re-rank with AVX2 and FMA through Go's experimental
// simd/archsimd package. Build with GOEXPERIMENT=simd. The package's API is
// not stable, so this file is kept small and the generic version stays the
// reference.

package vec

import (
	"simd/archsimd"
	"unsafe"
)

var useSIMD = archsimd.X86.AVX2() && archsimd.X86.FMA()

// SIMD reports whether the re-rank uses SIMD instructions.
var SIMD = useSIMD

func int8Dot(q []float32, b []byte) float32 {
	if !useSIMD || len(q)%8 != 0 || len(b) < len(q) {
		return int8DotGeneric(q, b)
	}
	ib := unsafe.Slice((*int8)(unsafe.Pointer(unsafe.SliceData(b))), len(b))
	acc := archsimd.Float32x8{}
	i := 0
	// Full 16-byte loads only; the last chunk falls through to scalar code.
	for ; i+16 <= len(ib) && i+8 <= len(q); i += 8 {
		x := archsimd.LoadInt8x16Slice(ib[i : i+16]).ExtendLo8ToInt32().ConvertToFloat32()
		acc = x.MulAdd(archsimd.LoadFloat32x8Slice(q[i:i+8]), acc)
	}
	var lanes [8]float32
	acc.Store(&lanes)
	s := lanes[0] + lanes[1] + lanes[2] + lanes[3] + lanes[4] + lanes[5] + lanes[6] + lanes[7]
	return s + int8DotGeneric(q[i:], b[i:])
}
