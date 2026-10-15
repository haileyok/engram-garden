package spacetest

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

// pdsRotationKey stands for the PDS's own rotation key in every account's
// PLC rotation keys.
const pdsRotationKey = "did:key:zPDSRotationKey"

// SetInvites makes createAccount need an invite code, with these codes
// and how many accounts each may still create.
func (n *Net) SetInvites(codes map[string]int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.invites = codes
}

// SetRotationKeys sets an account's PLC rotation keys, highest priority
// first.
func (n *Net) SetRotationKeys(did string, keys []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rotation[did] = keys
}

// RotationKeys is an account's PLC rotation keys.
func (n *Net) RotationKeys(did string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rotation[did]
}

// SignInKeys is the sign-in keys an account was created with.
func (n *Net) SignInKeys(did string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.signInKeys[did]
}

// createAccount is com.atproto.server.createAccount, with Cocoon's
// passwordless authKey: the new DID is did:plc:<the handle's first label>.
func (n *Net) createAccount(r *http.Request) (any, *xerr) {
	var body struct {
		Handle      string `json:"handle"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		InviteCode  string `json:"inviteCode"`
		RecoveryKey string `json:"recoveryKey"`
		AuthKey     string `json:"authKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, &xerr{400, "InvalidRequest", err.Error()}
	}
	if body.Password == "" && body.AuthKey == "" {
		return nil, &xerr{400, "InvalidPassword", "a password or authKey is required"}
	}
	for _, k := range []string{body.RecoveryKey, body.AuthKey} {
		if k == "" {
			continue
		}
		if _, err := atcrypto.ParsePublicDIDKey(k); err != nil {
			return nil, &xerr{400, "InvalidRequest", "not a did:key: " + k}
		}
	}
	label, _, _ := strings.Cut(body.Handle, ".")
	if label == "" {
		return nil, &xerr{400, "InvalidHandle", "bad handle"}
	}
	did := "did:plc:" + label
	n.mu.Lock()
	if n.invites != nil {
		if n.invites[body.InviteCode] <= 0 {
			n.mu.Unlock()
			return nil, &xerr{400, "InvalidInviteCode", "invite code not found or used up"}
		}
		n.invites[body.InviteCode]--
	}
	_, taken := n.accounts[did]
	n.mu.Unlock()
	if taken {
		return nil, &xerr{400, "HandleNotAvailable", "handle taken"}
	}
	n.NewAccount(did)
	keys := []string{pdsRotationKey}
	if body.RecoveryKey != "" {
		keys = append([]string{body.RecoveryKey}, keys...)
	}
	n.mu.Lock()
	n.rotation[did] = keys
	if body.AuthKey != "" {
		n.signInKeys[did] = append(n.signInKeys[did], body.AuthKey)
	}
	n.mu.Unlock()
	out := map[string]string{"did": did, "handle": body.Handle, "accessJwt": "", "refreshJwt": ""}
	return out, nil
}
