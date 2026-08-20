//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type bwrapRunner struct {
	once sync.Once
	ok   bool
}

func (b *bwrapRunner) Name() string          { return "bubblewrap" }
func (b *bwrapRunner) IsolatesNetwork() bool { return true }

func (b *bwrapRunner) Available() bool {
	b.once.Do(func() {
		if _, err := exec.LookPath("bwrap"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bwrap", "--die-with-parent", "--ro-bind", "/", "/", "--unshare-net", "/bin/true")
		b.ok = cmd.Run() == nil
	})
	return b.ok
}

func (b *bwrapRunner) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, fmt.Errorf("empty command")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "bwrap", bwrapArgs(spec)...)
	if spec.TmpDir != "" {
		cmd.Env = osEnvironWithTmp(spec.TmpDir)
	}
	return cmd, nil
}
