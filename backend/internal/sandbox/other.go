//go:build !linux && !darwin

package sandbox

func platform() Runner { return nil }
