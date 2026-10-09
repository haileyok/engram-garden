package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LLM calls an OpenAI-compatible chat completions endpoint and caches
// answers on disk, so a rerun repeats no calls.
type LLM struct {
	// URL is the API base, such as https://api.openai.com/v1.
	URL     string
	Model   string
	APIKey  string
	Headers map[string]string
	// CacheDir holds one file per answered prompt; empty disables caching.
	CacheDir string
	HTTP     *http.Client
	sem      chan struct{}
}

// LLMFromEnv configures an LLM from ENGRAM_EVAL_LLM_URL, _MODEL, _KEY
// (optional bearer token) and _HEADERS (optional "Name: value; Name2:
// value2"), allowing concurrency calls at once.
func LLMFromEnv(getenv func(string) string, cacheDir string, concurrency int) (*LLM, error) {
	l := &LLM{
		URL:      strings.TrimRight(getenv("ENGRAM_EVAL_LLM_URL"), "/"),
		Model:    getenv("ENGRAM_EVAL_LLM_MODEL"),
		APIKey:   getenv("ENGRAM_EVAL_LLM_KEY"),
		Headers:  map[string]string{},
		CacheDir: cacheDir,
		HTTP:     &http.Client{Timeout: 5 * time.Minute},
		sem:      make(chan struct{}, max(1, concurrency)),
	}
	if l.URL == "" || l.Model == "" {
		return nil, errors.New("set ENGRAM_EVAL_LLM_URL (an OpenAI-compatible API base, e.g. https://host/v1) and ENGRAM_EVAL_LLM_MODEL")
	}
	for _, h := range strings.Split(getenv("ENGRAM_EVAL_LLM_HEADERS"), ";") {
		name, value, ok := strings.Cut(h, ":")
		if ok && strings.TrimSpace(name) != "" {
			l.Headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return l, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// JSON asks for a JSON object and decodes it into out.
func (l *LLM) JSON(ctx context.Context, system, user string, out any) error {
	key := l.cacheKey(system, user)
	if l.CacheDir != "" {
		if b, err := os.ReadFile(filepath.Join(l.CacheDir, key)); err == nil {
			if json.Unmarshal(b, out) == nil {
				return nil
			}
		}
	}
	l.sem <- struct{}{}
	defer func() { <-l.sem }()
	var content string
	var err error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		content, err = l.call(ctx, system, user)
		if err == nil {
			if err = json.Unmarshal([]byte(stripFence(content)), out); err == nil {
				break
			}
			err = fmt.Errorf("model answered with invalid JSON: %w", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	if l.CacheDir != "" {
		if err := os.MkdirAll(l.CacheDir, 0o700); err == nil {
			_ = os.WriteFile(filepath.Join(l.CacheDir, key), []byte(stripFence(content)), 0o600)
		}
	}
	return nil
}

func (l *LLM) cacheKey(system, user string) string {
	h := sha256.New()
	for _, s := range []string{l.Model, system, user} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32] + ".json"
}

func (l *LLM) call(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":           l.Model,
		"messages":        []chatMessage{{"system", system}, {"user", user}},
		"response_format": map[string]string{"type": "json_object"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	for k, v := range l.Headers {
		req.Header.Set(k, v)
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat completions: %s: %.300s", resp.Status, b)
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", errors.New("chat completions: no choices")
	}
	return r.Choices[0].Message.Content, nil
}

// stripFence removes a Markdown code fence some models wrap JSON in.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}
