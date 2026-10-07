package lex

import (
	"errors"
	"strings"
)

// ConfigAction is a change the space authority makes to the space's model.
type ConfigAction string

const (
	// Declare sets the space's model, replacing any model change in progress.
	Declare ConfigAction = "declare"
	// StartNext starts moving the space to another model.
	StartNext ConfigAction = "next"
	// Promote finishes a move: the next model becomes the space's model.
	Promote ConfigAction = "promote"
	// CancelNext abandons a move.
	CancelNext ConfigAction = "cancel"
)

// ChangeConfig applies an action to the current config (nil when the space
// has none). m names the model for Declare and StartNext. Empty prefixes
// get the model's known task prefixes.
func ChangeConfig(cur *Config, action ConfigAction, m ModelInfo, documentPrefix, queryPrefix string) (Config, error) {
	switch action {
	case Declare:
		if !m.Valid() {
			return Config{}, errors.New("the model needs a name, a digest and dimensions")
		}
		cfg := Config{ModelInfo: m}
		cfg.DocumentPrefix, cfg.QueryPrefix = DefaultPrefixes(m.Model, documentPrefix, queryPrefix)
		return cfg, nil
	case StartNext:
		if cur == nil {
			return Config{}, errors.New("declare a model first")
		}
		if !m.Valid() {
			return Config{}, errors.New("the next model needs a name, a digest and dimensions")
		}
		if m.Key() == cur.ModelInfo.Key() {
			return Config{}, errors.New("that's already the space's model")
		}
		cfg := *cur
		cfg.Next = &m
		return cfg, nil
	case Promote:
		if cur == nil || cur.Next == nil {
			return Config{}, errors.New("no model change in progress")
		}
		cfg := Config{ModelInfo: *cur.Next}
		cfg.DocumentPrefix, cfg.QueryPrefix = DefaultPrefixes(cur.Next.Model, documentPrefix, queryPrefix)
		return cfg, nil
	case CancelNext:
		if cur == nil {
			return Config{}, errors.New("the space has no model")
		}
		cfg := *cur
		cfg.Next = nil
		return cfg, nil
	}
	return Config{}, errors.New("unknown action: use declare, next, promote or cancel")
}

// DefaultPrefixes fills in a model's known task prefixes unless given.
func DefaultPrefixes(model, documentPrefix, queryPrefix string) (string, string) {
	if documentPrefix == "" && queryPrefix == "" && strings.HasPrefix(model, "nomic-embed-text") {
		return "search_document: ", "search_query: "
	}
	return documentPrefix, queryPrefix
}
