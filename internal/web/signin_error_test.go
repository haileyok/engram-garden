package web

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

func TestSignInErrorMessage(t *testing.T) {
	t.Parallel()
	declined := &oauth.AuthRequestCallbackError{ErrorCode: "access_denied", ErrorDescription: "The user denied the request"}
	msg, result := signInErrorMessage(declined)
	if msg != "you declined to sign in" || result != "declined" {
		t.Fatalf("declined: %q %q", msg, result)
	}

	// What the server said is shown, so a person can tell what to fix.
	scope := &oauth.AuthRequestCallbackError{ErrorCode: "invalid_scope", ErrorDescription: "Unable to retrieve space declarations"}
	msg, result = signInErrorMessage(fmt.Errorf("callback: %w", scope))
	if result != "failed" || !strings.Contains(msg, "invalid_scope") || !strings.Contains(msg, "Unable to retrieve space declarations") {
		t.Fatalf("scope: %q %q", msg, result)
	}

	// With no description there is still the code.
	msg, _ = signInErrorMessage(&oauth.AuthRequestCallbackError{ErrorCode: "server_error"})
	if !strings.Contains(msg, "server_error") || strings.Contains(msg, "()") {
		t.Fatalf("no description: %q", msg)
	}

	// Other failures say nothing about their insides.
	msg, result = signInErrorMessage(errors.New("dial tcp 10.0.0.5:443: connection refused"))
	if msg != "signing in failed" || result != "failed" {
		t.Fatalf("other: %q %q", msg, result)
	}

	// The description comes from another server: no control characters, and not too long.
	nasty := &oauth.AuthRequestCallbackError{ErrorCode: "invalid_scope", ErrorDescription: "bad\x00\n\rthing " + strings.Repeat("x", 1000)}
	msg, _ = signInErrorMessage(nasty)
	if strings.ContainsAny(msg, "\x00\r\n") || len(msg) > 300 {
		t.Fatalf("not cleaned: %d bytes %q", len(msg), msg)
	}
}
