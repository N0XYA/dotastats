package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dotastats/internal/collector"
)

func TestRun(t *testing.T) {
	dataDir := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"version", []string{"version"}, 0, "dotastats dev", ""},
		{"help", []string{"help"}, 0, "Usage: dotastats", ""},
		{"unknown command", []string{"frobnicate"}, 2, "", `unknown command "frobnicate"`},
		{"prune empty database", []string{"prune", "--data-dir", dataDir}, 0, "Deleted raw JSON of 0 matches", ""},
		{"reextract empty database", []string{"reextract", "--data-dir", dataDir}, 0, "Rebuilt 0 matches", ""},
		{"bad flag", []string{"migrate", "--nope"}, 2, "", "flag provided but not defined"},
		{"serve flag help", []string{"-h"}, 0, "", "-no-browser"},
		{"serve refuses all interfaces", []string{"serve", "--addr", "0.0.0.0:8000", "--data-dir", dataDir}, 1, "", "not allowed"},
		{"migrate", []string{"migrate", "--data-dir", dataDir}, 0, "schema is at version ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.code {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.stdout)
			}
			if !strings.Contains(stderr.String(), tt.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.stderr)
			}
		})
	}
}

func TestMigrateCreatesDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "nested", "data")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"migrate", "--data-dir", dataDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "dota.db")); err != nil {
		t.Fatal(err)
	}
}

func TestCollectWhileServeRunning(t *testing.T) {
	dataDir := t.TempDir()
	// What serve holds for its whole life.
	lock, err := collector.AcquireLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	for _, args := range [][]string{
		{"collect"}, {"backfill"}, {"prune"}, {"reextract"}, {"serve", "--no-browser"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := args[0]
		code := run(append(args, "--data-dir", dataDir), &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "another dotastats process is using this data directory") {
			t.Errorf("%s with the lock held: exit %d, stderr %q", cmd, code, stderr.String())
		}
	}
}
