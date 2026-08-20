package sandbox

import (
	"context"
	"os/exec"
)

type pass struct{}

// Passthrough runs /bin/sh with no isolation. Available reports false so callers
// can tell the difference between a real backend and this fallback.
func Passthrough() Runner { return pass{} }

func (pass) Name() string          { return "none" }
func (pass) Available() bool       { return false }
func (pass) IsolatesNetwork() bool { return false }
func (p pass) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	return shCommand(ctx, spec), nil
}
