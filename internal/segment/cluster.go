package segment

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"

	"github.com/haileyok/engram-garden/internal/vec"
)

// clusterTable is a segment's cluster index: centers, and after the docs
// are sorted, each cluster's row range.
type clusterTable struct {
	dims    int
	centers [][]float32
	of      []uint16    // cluster of each doc, in final row order
	ranges  [][2]uint32 // per cluster: first row, count
}

// cluster groups docs around about √n centers (spherical k-means trained on
// a sample) and sorts docs by cluster.
func cluster(docs []Doc, dims int) *clusterTable {
	n := len(docs)
	k := max(2, min(MaxClusters, int(math.Round(math.Sqrt(float64(n))))))
	vectors := make([][]float32, n)
	for i, d := range docs {
		if d.Vector != nil {
			vectors[i] = d.Vector
		} else {
			vectors[i] = Dequantize(d.Int8, dims)
		}
	}
	r := rand.New(rand.NewPCG(uint64(n), uint64(dims)))

	// Train on a sample: enough points per center, without touching every
	// vector k times per iteration.
	sample := vectors
	if limit := 32 * k; n > limit {
		perm := r.Perm(n)[:limit]
		sample = make([][]float32, limit)
		for i, j := range perm {
			sample[i] = vectors[j]
		}
	}
	centers := make([][]float32, k)
	for i, j := range r.Perm(len(sample))[:k] {
		centers[i] = slices.Clone(sample[j])
	}
	assign := make([]int, len(sample))
	for range 6 {
		for i, v := range sample {
			assign[i] = nearestCenter(v, centers)
		}
		sums := make([][]float32, k)
		for i := range sums {
			sums[i] = make([]float32, dims)
		}
		for i, v := range sample {
			s := sums[assign[i]]
			for j, x := range v {
				s[j] += x
			}
		}
		for i, s := range sums {
			if vec.Normalize(s) {
				centers[i] = s
			} else {
				// Empty cluster: restart it on a random point.
				centers[i] = slices.Clone(sample[r.IntN(len(sample))])
			}
		}
	}

	// Assign every doc: shortlist centers by 1-bit distance, then pick the
	// nearest of the shortlist exactly.
	centerBits := make([][]byte, k)
	for i, c := range centers {
		centerBits[i] = vec.AppendBits(nil, c)
	}
	const shortlist = 8
	of := make([]uint16, n)
	type cand struct{ c, d int }
	cands := make([]cand, k)
	for i, d := range docs {
		for c := range centers {
			cands[c] = cand{c, vec.Hamming(d.Bits, centerBits[c])}
		}
		slices.SortFunc(cands, func(a, b cand) int { return a.d - b.d })
		best, bestScore := cands[0].c, float32(math.Inf(-1))
		for _, cd := range cands[:min(shortlist, k)] {
			if s := vec.Dot(vectors[i], centers[cd.c]); s > bestScore {
				best, bestScore = cd.c, s
			}
		}
		of[i] = uint16(best)
	}
	sortByCluster(docs, of)
	ct := &clusterTable{dims: dims, centers: centers, of: of, ranges: make([][2]uint32, k)}
	for row, c := range of {
		if ct.ranges[c][1] == 0 {
			ct.ranges[c][0] = uint32(row)
		}
		ct.ranges[c][1]++
	}
	return ct
}

func nearestCenter(v []float32, centers [][]float32) int {
	best, bestScore := 0, float32(math.Inf(-1))
	for i, c := range centers {
		if s := vec.Dot(v, c); s > bestScore {
			best, bestScore = i, s
		}
	}
	return best
}

func (ct *clusterTable) marshal() []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(ct.centers)))
	for _, r := range ct.ranges {
		b = binary.LittleEndian.AppendUint32(b, r[0])
		b = binary.LittleEndian.AppendUint32(b, r[1])
	}
	for _, c := range ct.centers {
		for _, x := range c {
			b = binary.LittleEndian.AppendUint32(b, math.Float32bits(x))
		}
	}
	return b
}

// Dequantize turns an int8 vector back into floats.
func Dequantize(iv []byte, dims int) []float32 {
	scale := math.Float32frombits(binary.LittleEndian.Uint32(iv[dims:]))
	out := make([]float32, dims)
	for i := range out {
		out[i] = float32(int8(iv[i])) * scale
	}
	return out
}
