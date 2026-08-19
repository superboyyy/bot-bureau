package config

import (
	"strings"
	"testing"
)

func TestNormalizeFetchHosts(t *testing.T) {
	got := NormalizeFetchHosts([]string{
		" https://GitHub.com/foo ",
		"github.com",
		"golang.org:443",
		"",
		"example.com/path",
	})
	want := []string{"github.com", "golang.org", "example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestHostAllowed(t *testing.T) {
	if !HostAllowed("anything.example", nil) || !HostAllowed("anything.example", []string{}) {
		t.Fatal("empty list must allow every host")
	}
	list := []string{"github.com"}
	if !HostAllowed("GitHub.com", list) {
		t.Fatal("match is case-insensitive")
	}
	if HostAllowed("gitlab.com", list) {
		t.Fatal("other hosts must be refused")
	}
	if err := HostAllowedErr("gitlab.com", list); err == nil || !strings.Contains(err.Error(), "gitlab.com") {
		t.Fatalf("error should name the host: %v", err)
	}
	if err := HostAllowedErr("github.com", list); err != nil {
		t.Fatal(err)
	}
}
