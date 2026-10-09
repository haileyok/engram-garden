package text

// Doc is what a memory contributes to the keyword index.
type Doc struct {
	// Length is the number of source tokens (words, compounds, opaque
	// tokens, CJK bigrams), not the number of terms they emit, so text full
	// of identifiers isn't penalized for its parts and stems.
	Length uint32
	// TF maps each term to the number of source tokens that emitted it.
	TF map[string]uint32
}

// AnalyzeMemory analyzes a memory's text, tags and source as one stream.
func AnalyzeMemory(text string, tags []string, source string) Doc {
	// About one distinct term per 4 bytes of text, measured on real memories.
	d := Doc{TF: make(map[string]uint32, len(text)/4+16)}
	var terms []string
	add := func(t *Token) {
		d.Length++
		terms = t.AppendTerms(terms[:0])
		for _, term := range terms {
			d.TF[term]++
		}
	}
	Tokenize(text, FieldText, 0, add)
	for i, tag := range tags {
		Tokenize(tag, FieldTag, i, add)
	}
	Tokenize(source, FieldSource, 0, add)
	return d
}
