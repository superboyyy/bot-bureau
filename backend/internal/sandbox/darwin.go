//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type seatbelt struct {
	once sync.Once
	ok   bool
}

func platform() Runner { return &seatbelt{} }

func (s *seatbelt) Name() string          { return "seatbelt" }
func (s *seatbelt) IsolatesNetwork() bool { return true }

func (s *seatbelt) Available() bool {
	s.once.Do(func() {
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", "(version 1)(allow default)", "/usr/bin/true")
		s.ok = cmd.Run() == nil
	})
	return s.ok
}

func (s *seatbelt) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", seatbeltProfile(spec), "/bin/sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	if spec.TmpDir != "" {
		cmd.Env = osEnvironWithTmp(spec.TmpDir)
	}
	return cmd, nil
}
