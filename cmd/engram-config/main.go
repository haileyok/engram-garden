// Command engram-config declares a memory space's embedding model. Run it as
// the space authority: it writes the garden.engram.config record (key
// "self") in the authority's repo in the space.
//
//	engram-config -show
//	engram-config -model nomic-embed-text          declare the model
//	engram-config -next mxbai-embed-large          start moving to a new model
//	engram-config -promote                         finish the move
//	engram-config -cancel-next                     abandon the move
//
// The model's digest comes from the local Ollama (or
// ENGRAM_EMBED_MODEL_DIGEST), and its dimensions from embedding a probe.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/haileyok/cocoon/space"

	"github.com/haileyok/engram-garden/internal/config"
	"github.com/haileyok/engram-garden/internal/embed"
	"github.com/haileyok/engram-garden/internal/lex"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "engram-config:", err)
		os.Exit(1)
	}
}

func run() error {
	show := flag.Bool("show", false, "print the current config")
	model := flag.String("model", "", "declare this model (an Ollama model name)")
	next := flag.String("next", "", "start a change to this model")
	promote := flag.Bool("promote", false, "make the next model the space's model")
	cancelNext := flag.Bool("cancel-next", false, "abandon a model change")
	docPrefix := flag.String("document-prefix", "", "text before stored memories when embedding (default: the model's convention)")
	queryPrefix := flag.String("query-prefix", "", "text before queries when embedding (default: the model's convention)")
	dims := flag.Int("dims", 0, "vector size, for the offline hashing provider only")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	spaceURI, err := config.Require("ENGRAM_SPACE")
	if err != nil {
		return err
	}
	ref, err := space.ParseRef(spaceURI)
	if err != nil {
		return fmt.Errorf("ENGRAM_SPACE: %w", err)
	}
	session, err := config.Login(ctx, config.Directory())
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	if session.AccountDID == nil || session.AccountDID.String() != ref.Authority {
		return fmt.Errorf("log in as the space authority (%s) to change its config", ref.Authority)
	}
	cur, err := current(ctx, session, spaceURI, ref.Authority)
	if err != nil {
		return err
	}
	if *show {
		if cur == nil {
			fmt.Println("no config: the space hasn't declared a model")
			return nil
		}
		out, _ := json.MarshalIndent(cur, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	var action lex.ConfigAction
	var m lex.ModelInfo
	switch {
	case *model != "":
		action = lex.Declare
		if m, err = describe(ctx, *model, *dims); err != nil {
			return err
		}
	case *next != "":
		if cur == nil {
			return errors.New("declare a model with -model first")
		}
		action = lex.StartNext
		if m, err = describe(ctx, *next, *dims); err != nil {
			return err
		}
	case *promote:
		action = lex.Promote
	case *cancelNext:
		action = lex.CancelNext
	default:
		flag.Usage()
		return errors.New("choose -show, -model, -next, -promote or -cancel-next")
	}
	cfg, err := lex.ChangeConfig(cur, action, m, *docPrefix, *queryPrefix)
	if err != nil {
		return err
	}
	body := map[string]any{
		"space": spaceURI, "repo": ref.Authority, "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey,
		"record": cfg.Record(time.Now()),
	}
	if err := session.Post(ctx, "com.atproto.space.putRecord", body, nil); err != nil {
		return fmt.Errorf("writing the config: %w", err)
	}
	fmt.Printf("space model: %s\n", cfg.ModelInfo)
	if cfg.Next != nil {
		fmt.Printf("moving to:   %s\nAgents running engram-mcp re-embed their memories in the background. Run -promote once the appview's getSpaceStatus shows enough coverage.\n", *cfg.Next)
	}
	return nil
}

// current reads the authority's config record, or nil.
func current(ctx context.Context, session *atclient.APIClient, spaceURI, authority string) (*lex.Config, error) {
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	params := map[string]any{"space": spaceURI, "repo": authority, "collection": lex.ConfigCollection, "rkey": lex.ConfigRkey}
	if err := session.Get(ctx, "com.atproto.space.getRecord", params, &out); err != nil {
		var ae *atclient.APIError
		if errors.As(err, &ae) && (ae.Name == "RecordNotFound" || ae.Name == "RepoNotFound") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the config: %w", err)
	}
	rec, err := atdata.UnmarshalJSON(out.Value)
	if err != nil {
		return nil, err
	}
	return lex.ParseConfig(rec)
}

// describe identifies a local model exactly: name, digest and dimensions.
func describe(ctx context.Context, name string, dims int) (lex.ModelInfo, error) {
	if config.Get("ENGRAM_EMBED_PROVIDER", "openai") == "hashing" {
		if dims <= 0 {
			return lex.ModelInfo{}, errors.New("-dims is required with the hashing provider")
		}
		return lex.ModelInfo{Model: name, ModelDigest: embed.HashingDigest, Dims: dims}, nil
	}
	base := config.Get("ENGRAM_EMBED_URL", "http://localhost:11434/v1")
	key := config.Get("ENGRAM_EMBED_API_KEY", "")
	client := &http.Client{Timeout: 60 * time.Second}
	digest := config.Get("ENGRAM_EMBED_MODEL_DIGEST", "")
	if digest == "" {
		d, err := embed.OllamaDigest(ctx, client, base, name)
		if err != nil {
			return lex.ModelInfo{}, err
		}
		if d == "" {
			return lex.ModelInfo{}, fmt.Errorf("%s isn't installed in Ollama: run ollama pull %s", name, name)
		}
		digest = d
	}
	n, err := embed.ProbeDims(ctx, base, key, name, client)
	if err != nil {
		return lex.ModelInfo{}, fmt.Errorf("embedding a probe with %s: %w", name, err)
	}
	return lex.ModelInfo{Model: name, ModelDigest: digest, Dims: n}, nil
}
