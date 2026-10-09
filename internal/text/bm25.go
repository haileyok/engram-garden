package text

import (
	"math"
	"sort"
)

// BM25 holds the ranking function's parameters.
type BM25 struct {
	K1, B float64
}

// DefaultBM25 are the starting parameters.
var DefaultBM25 = BM25{K1: 1.2, B: 0.75}

// IDF is Lucene's BM25 inverse document frequency for a term in df of n
// memories. It is always positive, which score bounds rely on.
func IDF(n, df uint64) float64 {
	if df > n {
		df = n
	}
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

// TF is the saturating term-frequency factor for a term occurring tf times
// in a memory of length dl, where memories average avgdl. It rises with tf,
// falls with dl, and stays below TFLimit.
func (p BM25) TF(tf, dl, avgdl float64) float64 {
	if tf <= 0 {
		return 0
	}
	if avgdl <= 0 {
		avgdl = 1
	}
	return tf * (p.K1 + 1) / (tf + p.K1*(1-p.B+p.B*dl/avgdl))
}

// TFLimit bounds TF for any tf and length.
func (p BM25) TFLimit() float64 { return p.K1 + 1 }

// Lengths are stored in one byte per memory. Codes 0..exactLengths-1 are
// exact; above that, each code is about 3% longer than the last. Decoding
// is increasing and EncodeLength rounds down, so the length a scorer uses
// is never more than the real one, and bounds computed from the smallest
// code in a block hold for every memory in it. The table is part of the
// segment format: changing it needs a new format version.
const exactLengths = 40

var lengthTable = func() [256]uint32 {
	var t [256]uint32
	for i := range exactLengths {
		t[i] = uint32(i)
	}
	for i := exactLengths; i < 256; i++ {
		t[i] = t[i-1] + max(1, t[i-1]/29)
	}
	return t
}()

// EncodeLength stores a memory's length in one byte, rounding down.
func EncodeLength(n uint32) byte {
	// The largest code whose length is at most n.
	i := sort.Search(256, func(i int) bool { return lengthTable[i] > n })
	return byte(i - 1)
}

// DecodeLength returns the length a code stands for.
func DecodeLength(b byte) uint32 { return lengthTable[b] }
