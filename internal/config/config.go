// Package config resolves startup settings from flags, environment
// variables and the executable's location. Everything else is configured
// through the UI and stored in the settings table.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
)

const (
	// EnvDataDir overrides the default data directory.
	EnvDataDir = "DOTASTATS_DATA"
	// EnvAPIKey is an optional OpenDota API key.
	EnvAPIKey = "OPENDOTA_API_KEY"
	// EnvDevCache=1 serves OpenDota GETs from <data-dir>/dev_cache and
	// saves new responses there. Development only.
	EnvDevCache = "DOTASTATS_DEV_CACHE"
	// DefaultAddr is where serve listens unless --addr says otherwise.
	DefaultAddr = "127.0.0.1:8000"
)

// AddDataDirFlag registers --data-dir on fs. Every subcommand that touches
// the database accepts it.
func AddDataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", "", "data directory (default: data/ next to the executable, or $"+EnvDataDir+")")
}

// DataDir returns the absolute data directory: the flag value if set, then
// $DOTASTATS_DATA, then data/ next to the executable.
func DataDir(flagValue string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return ResolveDataDir(flagValue, os.Getenv(EnvDataDir), exe)
}

// ResolveDataDir applies the data directory precedence to explicit inputs.
// Relative flag and env values are taken relative to the working directory.
func ResolveDataDir(flagValue, envValue, executable string) (string, error) {
	dir := flagValue
	if dir == "" {
		dir = envValue
	}
	if dir == "" {
		dir = filepath.Join(filepath.Dir(executable), "data")
	}
	return filepath.Abs(dir)
}

// DBPath returns the database file inside dataDir.
func DBPath(dataDir string) string {
	return filepath.Join(dataDir, "dota.db")
}

// ValidateAddr rejects listen addresses that would expose the server on
// every interface. The app has no passwords, so it must bind to loopback
// (or, later, a specific VPN interface address).
func ValidateAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if host == "" {
		return errors.New("address must include a host such as 127.0.0.1; listening on all interfaces is not allowed")
	}
	if host == "localhost" {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("address host %q must be an IP address or localhost", host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("address %q listens on all interfaces, which is not allowed; use 127.0.0.1", addr)
	}
	return nil
}
