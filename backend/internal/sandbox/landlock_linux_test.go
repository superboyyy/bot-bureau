//go:build linux

package sandbox

import "testing"

func TestLandlockHelperParseErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"-dir"},
		{"-tmp"},
		{"-write"},
		{"-nope"},
		{"--"},
	}
	for _, c := range cases {
		if err := landlockHelper(c); err == nil {
			t.Fatalf("expected error for %v", c)
		}
	}
}
