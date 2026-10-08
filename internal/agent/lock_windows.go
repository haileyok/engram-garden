//go:build windows

package agent

// lockFile doesn't lock on Windows: run one engram process per sign-in
// there.
func lockFile(string) (func(), error) { return func() {}, nil }
