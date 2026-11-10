package config

import (
	"path/filepath"
	"testing"
)

func TestResolveDataDir(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "bin", "dotastats")
	flagDir := filepath.Join(t.TempDir(), "from-flag")
	envDir := filepath.Join(t.TempDir(), "from-env")

	tests := []struct {
		name, flag, env, want string
	}{
		{"flag wins", flagDir, envDir, flagDir},
		{"env when no flag", "", envDir, envDir},
		{"next to executable by default", "", "", filepath.Join(filepath.Dir(exe), "data")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDataDir(tt.flag, tt.env, exe)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDataDirRelative(t *testing.T) {
	t.Chdir(t.TempDir())
	got, err := ResolveDataDir("rel", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || filepath.Base(got) != "rel" {
		t.Errorf("got %q, want an absolute path ending in rel", got)
	}
}

func TestValidateAddr(t *testing.T) {
	tests := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8000", true},
		{"127.0.0.1:0", true},
		{"localhost:8000", true},
		{"[::1]:8000", true},
		{"10.8.0.1:8000", true},
		{"0.0.0.0:8000", false},
		{"[::]:8000", false},
		{":8000", false},
		{"127.0.0.1", false},
		{"example.com:8000", false},
	}
	for _, tt := range tests {
		err := ValidateAddr(tt.addr)
		if (err == nil) != tt.ok {
			t.Errorf("ValidateAddr(%q) = %v, want ok=%v", tt.addr, err, tt.ok)
		}
	}
}
