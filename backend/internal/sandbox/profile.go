package sandbox

import (
	"fmt"
	"strings"
)

func seatbeltProfile(spec Spec) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process*)\n(allow signal)\n(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n(allow ipc-posix-shm)\n(allow ipc-posix-sem)\n")
	b.WriteString("(allow system-audit)\n(allow file-read*)\n(allow file-ioctl)\n")
	b.WriteString("(allow file-write-data (literal \"/dev/null\") (literal \"/dev/zero\"))\n")
	b.WriteString("(allow file-write* (subpath \"/private/tmp\") (subpath \"/tmp\"))\n")
	for _, p := range spec.Writable {
		if p == "" {
			continue
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath \"%s\"))\n", sbplPath(p))
	}
	if spec.TmpDir != "" {
		fmt.Fprintf(&b, "(allow file-write* (subpath \"%s\"))\n", sbplPath(spec.TmpDir))
	}
	if spec.Dir != "" {
		fmt.Fprintf(&b, "(allow file-write* (subpath \"%s\"))\n", sbplPath(spec.Dir))
	}
	for _, p := range spec.ReadOnly {
		if p == "" {
			continue
		}
		fmt.Fprintf(&b, "(deny file-write* (subpath \"%s\"))\n", sbplPath(p))
	}
	b.WriteString("(deny network*)\n(deny network-outbound)\n(deny network-inbound)\n(deny network-bind)\n")
	return b.String()
}

func sbplPath(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `"`, `\"`)
	return p
}

func bwrapArgs(spec Spec) []string {
	args := []string{
		"--die-with-parent",
		"--unshare-net",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}
	bind := map[string]bool{}
	addBind := func(flag, p string) {
		if p == "" || bind[p] {
			return
		}
		bind[p] = true
		args = append(args, flag, p, p)
	}
	addBind("--bind", spec.Dir)
	addBind("--bind", spec.TmpDir)
	for _, p := range spec.Writable {
		addBind("--bind", p)
	}
	for _, p := range spec.ReadOnly {
		if p == "" {
			continue
		}
		args = append(args, "--ro-bind", p, p)
	}
	if spec.Dir != "" {
		args = append(args, "--chdir", spec.Dir)
	}
	args = append(args, "/bin/sh", "-c", spec.Command)
	return args
}
