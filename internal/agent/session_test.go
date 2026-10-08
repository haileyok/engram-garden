package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestCallbackFromBrowser(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := io.Pipe() // nothing pasted
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/callback?state=s1&iss=https://pds.test&code=c")
		if err == nil {
			resp.Body.Close()
		}
	}()
	q, err := waitForCallback(context.Background(), ln, pr)
	if err != nil || q.Get("state") != "s1" || q.Get("code") != "c" {
		t.Fatalf("callback: %v %v", q, err)
	}
}

// TestCallbackPasted: a browser on another machine can't reach this one's
// loopback address, so the address it ends on can be pasted instead.
func TestCallbackPasted(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("not a url\nhttp://127.0.0.1:1234/callback?state=s2&iss=https%3A%2F%2Fpds.test&code=c2\n")
	q, err := waitForCallback(context.Background(), ln, in)
	if err != nil || q.Get("state") != "s2" || q.Get("iss") != "https://pds.test" {
		t.Fatalf("pasted: %v %v", q, err)
	}
}

func TestExplainExpiredSignIn(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errors.New("xrpc: token refresh failed (HTTP 400): invalid_grant"),
		errors.New("resuming the sign-in: session: not found"),
	} {
		if got := Explain(err); !errors.Is(got, ErrSignInExpired) || !strings.Contains(got.Error(), "engram login") {
			t.Fatalf("Explain(%v) = %v", err, got)
		}
	}
	other := errors.New("connection refused")
	if Explain(other) != other || Explain(nil) != nil {
		t.Fatal("explained an unrelated error")
	}
}

func TestGrantsMemories(t *testing.T) {
	t.Parallel()
	// As Cocoon issues it.
	if !grantsMemories([]string{"atproto", "space:garden.engram.space?collection=garden.engram.memory&action=read&action=create&action=update&action=delete"}) {
		t.Fatal("refused the memory grant")
	}
	if grantsMemories([]string{"atproto"}) || grantsMemories([]string{"space:other.space?collection=garden.engram.memory"}) ||
		grantsMemories([]string{"space:garden.engram.space?collection=garden.engram.memoryfoo&action=read&action=create"}) ||
		grantsMemories([]string{"space:garden.engram.space?collection=garden.engram.memory&action=read"}) {
		t.Fatal("accepted a grant without memory spaces")
	}
}
