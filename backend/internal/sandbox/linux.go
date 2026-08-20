//go:build linux

package sandbox

func platform() Runner {
	b := &bwrapRunner{}
	if b.Available() {
		return b
	}
	l := &landlockRunner{}
	if l.Available() {
		return l
	}
	return nil
}
