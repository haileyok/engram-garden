package lex

import "testing"

func TestChangeConfig(t *testing.T) {
	t.Parallel()
	nomic := ModelInfo{Model: "nomic-embed-text", ModelDigest: "sha256:aa", Dims: 768}
	mxbai := ModelInfo{Model: "mxbai-embed-large", ModelDigest: "sha256:bb", Dims: 1024}

	if _, err := ChangeConfig(nil, StartNext, mxbai, "", ""); err == nil {
		t.Fatal("next without a model")
	}
	if _, err := ChangeConfig(nil, Declare, ModelInfo{Model: "x"}, "", ""); err == nil {
		t.Fatal("declared an incomplete model")
	}
	cfg, err := ChangeConfig(nil, Declare, nomic, "", "")
	if err != nil || cfg.ModelInfo != nomic || cfg.DocumentPrefix != "search_document: " || cfg.QueryPrefix != "search_query: " {
		t.Fatalf("declare: %+v %v", cfg, err)
	}
	if _, err := ChangeConfig(&cfg, StartNext, nomic, "", ""); err == nil {
		t.Fatal("moved to the same model")
	}
	moving, err := ChangeConfig(&cfg, StartNext, mxbai, "", "")
	if err != nil || moving.Next == nil || *moving.Next != mxbai || moving.ModelInfo != nomic {
		t.Fatalf("next: %+v %v", moving, err)
	}
	cancelled, err := ChangeConfig(&moving, CancelNext, ModelInfo{}, "", "")
	if err != nil || cancelled.Next != nil || cancelled.ModelInfo != nomic {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if _, err := ChangeConfig(&cancelled, Promote, ModelInfo{}, "", ""); err == nil {
		t.Fatal("promoted with no change in progress")
	}
	promoted, err := ChangeConfig(&moving, Promote, ModelInfo{}, "", "")
	if err != nil || promoted.ModelInfo != mxbai || promoted.Next != nil || promoted.DocumentPrefix != "" {
		t.Fatalf("promote: %+v %v", promoted, err)
	}
	if _, err := ChangeConfig(&cfg, "bogus", nomic, "", ""); err == nil {
		t.Fatal("accepted an unknown action")
	}
}
