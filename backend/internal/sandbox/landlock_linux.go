//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const helperArg = "-botbureau-sandbox"

func init() {
	if len(os.Args) > 1 && os.Args[1] == helperArg {
		if err := landlockHelper(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox helper: %v\n", err)
			os.Exit(127)
		}
		os.Exit(0)
	}
}

type landlockRunner struct{}

func (landlockRunner) Name() string          { return "landlock" }
func (landlockRunner) IsolatesNetwork() bool { return false }

func (landlockRunner) Available() bool {
	_, err := landlockABI()
	return err == nil
}

func (landlockRunner) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, fmt.Errorf("empty command")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{helperArg}
	if spec.Dir != "" {
		args = append(args, "-dir", spec.Dir)
	}
	if spec.TmpDir != "" {
		args = append(args, "-tmp", spec.TmpDir)
	}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		args = append(args, "-write", p)
	}
	add(spec.Dir)
	add(spec.TmpDir)
	for _, p := range spec.Writable {
		add(p)
	}
	args = append(args, "--", "/bin/sh", "-c", spec.Command)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = spec.Dir
	if spec.TmpDir != "" {
		cmd.Env = osEnvironWithTmp(spec.TmpDir)
	}
	return cmd, nil
}

func landlockHelper(args []string) error {
	var dir, tmp string
	var writes []string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-dir":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing -dir value")
			}
			dir = args[i]
		case "-tmp":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing -tmp value")
			}
			tmp = args[i]
		case "-write":
			i++
			if i >= len(args) {
				return fmt.Errorf("missing -write value")
			}
			writes = append(writes, args[i])
		case "--":
			rest = args[i+1:]
			i = len(args)
		default:
			return fmt.Errorf("unknown helper flag %s", args[i])
		}
	}
	if len(rest) == 0 {
		return fmt.Errorf("missing command")
	}
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	if tmp != "" {
		_ = os.Setenv("TMPDIR", tmp)
	}
	if err := restrictWrites(writes); err != nil {
		return err
	}
	bin := rest[0]
	if !filepath.IsAbs(bin) {
		p, err := exec.LookPath(bin)
		if err != nil {
			return err
		}
		bin = p
	}
	return syscall.Exec(bin, rest, os.Environ())
}

func restrictWrites(writes []string) error {
	access, err := landlockWriteAccess()
	if err != nil {
		return err
	}
	attr := unix.LandlockRulesetAttr{Access_fs: uint64(access)}
	fd, err := landlockCreate(&attr)
	if err != nil {
		return fmt.Errorf("landlock_create_ruleset: %w", err)
	}
	defer unix.Close(fd)
	for _, p := range writes {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		dirfd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		rule := unix.LandlockPathBeneathAttr{
			Allowed_access: uint64(access),
			Parent_fd:      int32(dirfd),
		}
		err = landlockAddRule(fd, &rule)
		_ = unix.Close(dirfd)
		if err != nil {
			return fmt.Errorf("landlock_add_rule %s: %w", p, err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
	}
	if err := landlockRestrict(fd); err != nil {
		return fmt.Errorf("landlock_restrict_self: %w", err)
	}
	return nil
}

func landlockABI() (int, error) {
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

func landlockCreate(attr *unix.LandlockRulesetAttr) (int, error) {
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(attr)), unsafe.Sizeof(*attr), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

func landlockAddRule(fd int, rule *unix.LandlockPathBeneathAttr) error {
	_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(fd), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(rule)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func landlockRestrict(fd int) error {
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func landlockWriteAccess() (uintptr, error) {
	abi, err := landlockABI()
	if err != nil {
		return 0, err
	}
	access := uintptr(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		access |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		access |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return access, nil
}
