// Package sandbox wraps bash in an OS isolation backend.
//
// Approval (config.PermLevel) is a separate knob: this package only answers
// what the process may touch. See docs/sandbox.md.
package sandbox

import (
	"context"
	"os/exec"
	"strings"
)

// Spec is one bash invocation to wrap.
type Spec struct {
	Command  string
	Dir      string
	Writable []string
	ReadOnly []string // re-applied after Writable (e.g. .git)
	TmpDir   string
}

// Runner builds an *exec.Cmd that enforces Spec.
type Runner interface {
	Name() string
	Available() bool
	IsolatesNetwork() bool
	Command(ctx context.Context, spec Spec) (*exec.Cmd, error)
}

// Detect picks the strongest backend that works on this machine.
func Detect() Runner {
	if r := platform(); r != nil && r.Available() {
		return r
	}
	return Passthrough()
}

func shCommand(ctx context.Context, spec Spec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	if spec.TmpDir != "" {
		cmd.Env = osEnvironWithTmp(spec.TmpDir)
	}
	return cmd
}

func osEnvironWithTmp(tmp string) []string {
	out := make([]string, 0, 16)
	seenTmp := false
	for _, e := range execEnv() {
		if strings.HasPrefix(e, "TMPDIR=") {
			out = append(out, "TMPDIR="+tmp)
			seenTmp = true
			continue
		}
		out = append(out, e)
	}
	if !seenTmp {
		out = append(out, "TMPDIR="+tmp)
	}
	return out
}
