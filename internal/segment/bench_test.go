package segment

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// BenchmarkWrite100k builds a synthetic 100,000-memory segment (768
// dimensions, ~500 characters of text each) and reports its size, replacing
// the design doc's estimates with measurements.
func BenchmarkWrite100k(b *testing.B) {
	r := rand.New(rand.NewPCG(9, 9))
	words := strings.Fields("the deploy workflow takes a sha and every service ships together while agents record decisions preferences lessons and gotchas for later recall across projects")
	const n, dims = 100_000, 768
	docs := make([]Doc, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range docs {
		var sb strings.Builder
		for sb.Len() < 500 {
			sb.WriteString(words[r.IntN(len(words))])
			sb.WriteByte(' ')
		}
		docs[i] = Doc{
			ID: uint32(i), Author: fmt.Sprintf("did:plc:agent%02d", i%40), Rkey: fmt.Sprintf("3m%011d", i),
			CID: fmt.Sprintf("bafyreib%052d", i), Text: sb.String(), Source: "https://example.com/pr/" + fmt.Sprint(i%997),
			Tags: []string{fmt.Sprintf("project%d", i%25), "decision"}, CreatedAt: base.Add(time.Duration(i) * time.Minute),
			IndexedAt: base, Vector: randVec(r, dims),
		}
	}
	b.ResetTimer()
	for b.Loop() {
		data, info, err := Bytes(docs, WriteOptions{Dims: dims})
		if err != nil {
			b.Fatal(err)
		}
		rd, _ := Open(context.Background(), BytesReader(data))
		b.ReportMetric(float64(info.Bytes)/1e6, "MB")
		b.ReportMetric(float64(info.Bytes)/n, "bytes/memory")
		b.ReportMetric(float64(rd.h.sections[secBits].len)/1e6, "1bit-MB")
		b.ReportMetric(float64(rd.h.sections[secDocs].len)/1e6, "docs-MB")
	}
}
