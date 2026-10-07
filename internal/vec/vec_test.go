package vec

import (
	"math"
	"math/rand/v2"
	"testing"
)

func randVec(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	Normalize(v)
	return v
}

func TestF16RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []float32{0, 1, -1, 0.5, 65504, -65504, 6.1035156e-05, 5.9604645e-08, 0.1, -0.333}
	for _, x := range cases {
		got, err := DecodeF16(EncodeF16([]float32{x}))
		if err != nil {
			t.Fatal(err)
		}
		if d := math.Abs(float64(got[0] - x)); d > math.Abs(float64(x))*1e-3+1e-7 {
			t.Errorf("%v round-tripped to %v", x, got[0])
		}
	}
	// Exhaustive: every finite half decodes and re-encodes to itself.
	for h := 0; h < 1<<16; h++ {
		if uint16(h)&0x7c00 == 0x7c00 {
			continue
		}
		f := f16ToF32(uint16(h))
		if back := f32ToF16(f); back != uint16(h) {
			t.Fatalf("half %#04x -> %v -> %#04x", h, f, back)
		}
	}
	if _, err := DecodeF16([]byte{0, 0x7c}); err == nil {
		t.Fatal("infinity accepted")
	}
	if _, err := DecodeF16([]byte{1}); err == nil {
		t.Fatal("odd length accepted")
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	v := []float32{3, 4}
	if !Normalize(v) || math.Abs(float64(v[0]-0.6)) > 1e-6 {
		t.Fatalf("got %v", v)
	}
	if Normalize([]float32{0, 0}) {
		t.Fatal("zero vector normalized")
	}
}

func TestQuantizedOrderingTracksFloat(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	const dims = 768
	q := randVec(r, dims)
	qb := AppendBits(nil, q)
	var worst float64
	for range 200 {
		v := randVec(r, dims)
		// Bias some vectors toward the query so similarities spread out.
		for i := range v {
			v[i] += q[i] * float32(r.Float64())
		}
		Normalize(v)
		exact := Dot(q, v)
		approx := Int8Dot(q, AppendInt8(nil, v))
		worst = max(worst, math.Abs(float64(exact-approx)))
		// Hamming distance tracks angle: cos ≈ cos(π·h/d).
		h := Hamming(qb, AppendBits(nil, v))
		est := math.Cos(math.Pi * float64(h) / dims)
		if math.Abs(est-float64(exact)) > 0.2 {
			t.Fatalf("1-bit estimate %.3f vs exact %.3f", est, exact)
		}
	}
	if worst > 0.01 {
		t.Fatalf("int8 dot error up to %.4f", worst)
	}
}

func TestInt8DotMatchesGeneric(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(3, 4))
	for _, dims := range []int{8, 13, 256, 768, 1536} {
		q, v := randVec(r, dims), randVec(r, dims)
		iv := AppendInt8(nil, v)
		got := int8Dot(q, iv[:dims])
		want := int8DotGeneric(q, iv[:dims])
		if math.Abs(float64(got-want)) > 1e-3 {
			t.Fatalf("dims %d: %v vs %v", dims, got, want)
		}
	}
}

func TestBitsLayout(t *testing.T) {
	t.Parallel()
	if BitBytes(768) != 96 || BitBytes(1) != 8 || BitBytes(65) != 16 {
		t.Fatal("BitBytes")
	}
	b := AppendBits(nil, []float32{1, -1, 0, 2})
	if len(b) != 8 || b[0] != 0b1001 {
		t.Fatalf("bits %08b", b[0])
	}
}

func BenchmarkHamming768(b *testing.B) {
	r := rand.New(rand.NewPCG(5, 6))
	q := AppendBits(nil, randVec(r, 768))
	const n = 100_000
	data := make([]byte, 0, n*96)
	for range n {
		data = AppendBits(data, randVec(r, 768))
	}
	b.ResetTimer()
	for b.Loop() {
		s := 0
		for i := 0; i < len(data); i += 96 {
			s += Hamming(q, data[i:i+96])
		}
		_ = s
	}
	b.ReportMetric(float64(n*b.N)/b.Elapsed().Seconds(), "vectors/s")
}

func BenchmarkInt8Dot768(b *testing.B) {
	r := rand.New(rand.NewPCG(7, 8))
	q := randVec(r, 768)
	const n = 10_000
	data := make([]byte, 0, n*772)
	for range n {
		data = AppendInt8(data, randVec(r, 768))
	}
	b.ResetTimer()
	for b.Loop() {
		var s float32
		for i := 0; i < len(data); i += 772 {
			s += Int8Dot(q, data[i:i+772])
		}
		_ = s
	}
	b.ReportMetric(float64(n*b.N)/b.Elapsed().Seconds(), "vectors/s")
}
