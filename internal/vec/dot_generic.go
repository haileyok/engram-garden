//go:build !(goexperiment.simd && amd64)

package vec

func int8Dot(q []float32, b []byte) float32 { return int8DotGeneric(q, b) }

// SIMD reports whether the re-rank uses SIMD instructions.
const SIMD = false
