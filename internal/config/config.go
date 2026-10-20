// Package config owns the agent's on-disk settings and its bearer token.
//
// The file lives in ProgramData, not a user profile: the agent normally runs as
// a Windows service under LocalSystem, which has no useful profile directory,
// and the token has to survive a cashier logging out.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ServiceName is the Windows service key. It is also the ProgramData folder.
const ServiceName = "Thr1vePrint"

// DefaultPort is deliberately not one of QZ Tray's (8181-8485), so a machine
// mid-migration can run both without either stealing the other's socket.
const DefaultPort = 9101

// Config is the whole of the agent's configuration.
type Config struct {
	// Port on 127.0.0.1. Never a bind address: the loopback-only bind is a
	// security property, not a preference, so it is not configurable.
	Port int `json:"port"`

	// Token is the bearer credential the browser must present. Generated once
	// at install; rotating it means clearing this field and restarting.
	Token string `json:"token"`

	// AllowedOrigins is the exact-match allowlist for the Origin header. A
	// request whose Origin is absent or unlisted cannot print.
	AllowedOrigins []string `json:"allowedOrigins"`

	// LogPath is where the log goes. Empty means alongside the config.
	LogPath string `json:"logPath,omitempty"`

	// Relay, when set, makes this agent a print station: it also polls the
	// server for jobs queued by devices that have no agent of their own (a
	// waiter's tablet). Written by `agent pair`. Absent in every config from
	// before 1.1.0, which simply means the relay is off.
	Relay *Relay `json:"relay,omitempty"`
}

// Relay is the station's link to the app.
type Relay struct {
	// ServerURL is the app's origin, e.g. https://pos.example.com. HTTPS
	// except for a loopback dev server.
	ServerURL string `json:"serverUrl"`

	// Token is the station credential the server issued at pairing,
	// `<station id>.<secret>`. Unlike Token above it IS a server credential:
	// it lets whoever holds it take this venue's print jobs. Never logged.
	Token string `json:"token"`

	// StationName is for `agent status` only.
	StationName string `json:"stationName,omitempty"`
}

// DefaultOrigins is what a fresh install trusts: production, plus the dev
// server. Exact origins - no wildcards, no suffix matching.
var DefaultOrigins = []string{
	"https://thr1vefitstudio.com",
	"https://www.thr1vefitstudio.com",
	"https://localhost:5173",
	"https://127.0.0.1:5173",
	"http://localhost:5173",
	"http://127.0.0.1:5173",
}

// Dir is the directory holding config.json and the log.
func Dir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, ServiceName)
		}
		return filepath.Join("C:\\ProgramData", ServiceName)
	}
	// Dev machines. Same shape so the code paths do not diverge.
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".thr1ve-print")
	}
	return filepath.Join(home, ".thr1ve-print")
}

// Path is the config file itself.
func Path() string { return filepath.Join(Dir(), "config.json") }

// NewToken mints a bearer token: 32 bytes of crypto/rand, base64url, unpadded.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func defaults() *Config {
	return &Config{
		Port:           DefaultPort,
		AllowedOrigins: append([]string(nil), DefaultOrigins...),
	}
}

// Load reads config.json. A missing file is not an error - it yields the
// defaults with an empty token, which EnsureToken then fills.
func Load() (*Config, error) {
	cfg := defaults()

	raw, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", Path(), err)
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", Path(), err)
	}

	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = DefaultPort
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = append([]string(nil), DefaultOrigins...)
	}
	return cfg, nil
}

// Save writes config.json, creating the directory if needed. The file is 0600:
// on Windows the ACL is applied by install.ps1, but on a dev Mac this mode is
// the only thing keeping the token out of another account's reach.
func (c *Config) Save() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", Dir(), err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(Path(), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", Path(), err)
	}
	return nil
}

// EnsureToken fills in a token if there is none and persists it. It never
// replaces an existing one: re-running the installer must not silently
// invalidate the value a cashier already pasted into the browser.
func (c *Config) EnsureToken() (created bool, err error) {
	if strings.TrimSpace(c.Token) != "" {
		return false, nil
	}
	tok, err := NewToken()
	if err != nil {
		return false, err
	}
	c.Token = tok
	return true, c.Save()
}

// AllowOrigin adds an origin to the allowlist if it is not there yet. Used by
// `agent pair`: the allowlist is persisted, so a new default never reaches an
// agent installed before it, and pairing is the one step every venue runs.
func (c *Config) AllowOrigin(origin string) bool {
	if c.OriginAllowed(origin) {
		return false
	}
	c.AllowedOrigins = append(c.AllowedOrigins, origin)
	return true
}

// LogFile is the resolved log path.
func (c *Config) LogFile() string {
	if c.LogPath != "" {
		return c.LogPath
	}
	return filepath.Join(Dir(), "agent.log")
}

// OriginAllowed reports whether an Origin header may talk to this agent.
// Exact match only, and an empty Origin is never allowed - that is a
// non-browser caller, and the browser is the only client this agent has.
func (c *Config) OriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range c.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}
