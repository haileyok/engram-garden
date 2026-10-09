package text

import "testing"

// benchMemory is a made-up memory shaped like real ones: about 900
// characters of prose with identifiers, paths, a DID and a record URI.
const benchMemory = `Deployed the appview fix for reembed today. The Python client read a uri
from com.atproto.space.listRecords items, but cocoon only sends collection, rkey,
cid and value, so Agent.reembed raised KeyError against a real server while the Go
version silently skipped every record (parseURI of an empty string failed). Both
now use the rkey. The fakes in internal/spacestore/spacetest and python/tests/fakes.py
returned a uri, which hid it. Verified against did:plc:ewvi7nxzyoun6zhxrhs64oiz with
1,176 memories: reembed paged every record and rewrote 0. Follow-up: bump the pin in
Victrola's pyproject.toml to 83174b7 after the PR merges, then restart. Related post:
at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/app.bsky.feed.post/3mdj4meipr722. The
ModelMismatch path still re-reads the config once before failing. Café test: naïve
résumé handling looks right.`

var benchTags = []string{"engram", "deploy", "python-client"}

func BenchmarkAnalyzeMemory(b *testing.B) {
	b.SetBytes(int64(len(benchMemory)))
	b.ReportAllocs()
	for b.Loop() {
		AnalyzeMemory(benchMemory, benchTags, "https://github.com/haileyok/engram-garden/pull/15")
	}
}

func BenchmarkParseQuery(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		ParseQuery("why does reembed fail on listRecords against cocoon")
	}
}
