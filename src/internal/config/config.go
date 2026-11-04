// Package config owns gateway defaults and the shared model catalog.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"unbundle-go/src/internal/jsonutil"
)

const (
	SchemaVersion                = 1
	CLIBaseline                  = "2.1.316"
	MaxConfigBytes         int64 = 1 << 20
	DefaultMaxRequestBytes int64 = 8 << 20
)

type Config struct {
	Version         int      `json:"version"`
	Profile         string   `json:"profile"`
	Listen          string   `json:"listen"`
	TokenFile       string   `json:"token_file"`
	StateDir        string   `json:"state_dir"`
	CLI             *CLI     `json:"cli"`
	MaxRequestBytes int64    `json:"max_request_bytes"`
	DefaultModel    string   `json:"default_model"`
	Models          []Model  `json:"models"`
	Runtime         *Runtime `json:"runtime"`
	Logging         *Logging `json:"logging"`
}

type Logging struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

type Runtime struct {
	MaxActiveRequests         int   `json:"max_active_requests"`
	MaxSessions               int   `json:"max_sessions"`
	MaxSessionRecords         int   `json:"max_session_records"`
	RequestTimeoutSeconds     int   `json:"request_timeout_seconds"`
	WriteTimeoutSeconds       int   `json:"write_timeout_seconds"`
	MaxOutputBytes            int64 `json:"max_output_bytes"`
	ToolWaitTimeoutSeconds    int   `json:"tool_wait_timeout_seconds"`
	SessionIdleTimeoutSeconds int   `json:"session_idle_timeout_seconds"`
}

type CLI struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// Model is a configured route, not proof of account access or CLI capability.
// OutputTokenLimit is a deployment policy, not a claim about the model maximum.
type Model struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	CLIModel         string   `json:"cli_model"`
	ContextWindow    *int64   `json:"context_window,omitempty"`
	OutputTokenLimit int64    `json:"output_token_limit"`
	Efforts          []string `json:"efforts,omitempty"`
	WebSearch        bool     `json:"web_search,omitempty"`
}

func Defaults() Config {
	return Config{
		Version: SchemaVersion, Profile: "unbundle-go", Listen: "127.0.0.1:38191", StateDir: "data/unbundle-go",
		CLI: &CLI{Path: "claude", Version: CLIBaseline}, MaxRequestBytes: DefaultMaxRequestBytes,
		Logging: &Logging{Level: "info", Format: "json"},
		Runtime: &Runtime{MaxActiveRequests: 5, MaxSessions: 5, MaxSessionRecords: 128, RequestTimeoutSeconds: 300, WriteTimeoutSeconds: 15, MaxOutputBytes: 32 << 20, ToolWaitTimeoutSeconds: 1800, SessionIdleTimeoutSeconds: 600},
	}
}

// Load applies defaults then one explicit JSON file. Environment variables and
// request fields cannot override deployment configuration. Explicit zero values
// remain zero and fail validation where the setting must be positive.
func Load(r io.Reader) (Config, error) {
	c := Defaults()
	if err := jsonutil.Decode(r, MaxConfigBytes, &c); err != nil {
		return Config{}, fmt.Errorf("configuration: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// LoadFile resolves relative file paths against the configuration's directory.
// It does not open credentials, invoke the CLI, bind a port or contact a model.
func LoadFile(name string) (Config, error) {
	f, err := os.Open(name)
	if err != nil {
		return Config{}, errors.New("could not open configuration file")
	}
	defer f.Close()
	c, err := Load(f)
	if err != nil {
		return Config{}, err
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return Config{}, errors.New("could not resolve configuration directory")
	}
	if !filepath.IsAbs(c.TokenFile) {
		c.TokenFile = filepath.Join(filepath.Dir(abs), c.TokenFile)
	}
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(filepath.Dir(abs), c.StateDir)
	}
	if !filepath.IsAbs(c.CLI.Path) && strings.ContainsAny(c.CLI.Path, `/\`) {
		c.CLI.Path = filepath.Join(filepath.Dir(abs), c.CLI.Path)
	}
	return c, nil
}

func (c Config) Validate() error {
	fail := func(field string) error { return fmt.Errorf("invalid configuration field: %s", field) }
	if c.Version != SchemaVersion {
		return fail("version")
	}
	if !profileName(c.Profile) {
		return fail("profile")
	}
	addr, err := netip.ParseAddrPort(c.Listen)
	if err != nil || !addr.Addr().IsLoopback() || addr.Port() == 0 {
		return fail("listen (requires a loopback IP and nonzero port)")
	}
	if !plain(c.TokenFile) {
		return fail("token_file")
	}
	if !plain(c.StateDir) {
		return fail("state_dir")
	}
	if c.Logging == nil || !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Logging.Level) || !slices.Contains([]string{"json", "text"}, c.Logging.Format) {
		return fail("logging (requires level debug/info/warn/error and format json/text)")
	}
	if c.CLI == nil || !plain(c.CLI.Path) || c.CLI.Version != CLIBaseline {
		return fail("cli (requires the baseline version 2.1.316)")
	}
	if c.MaxRequestBytes < 1024 || c.MaxRequestBytes > 256<<20 {
		return fail("max_request_bytes")
	}
	if c.Runtime == nil || c.Runtime.RequestTimeoutSeconds < 1 || c.Runtime.RequestTimeoutSeconds > 3600 ||
		c.Runtime.WriteTimeoutSeconds < 1 || c.Runtime.WriteTimeoutSeconds > 60 ||
		c.Runtime.MaxOutputBytes < 1024 || c.Runtime.MaxOutputBytes > 256<<20 {
		return fail("runtime")
	}
	if c.Runtime.ToolWaitTimeoutSeconds < 1 || c.Runtime.ToolWaitTimeoutSeconds > 86400 || c.Runtime.SessionIdleTimeoutSeconds < 1 || c.Runtime.SessionIdleTimeoutSeconds > 86400 {
		return fail("runtime session timeouts")
	}
	if c.Runtime.MaxActiveRequests < 1 || c.Runtime.MaxActiveRequests > 64 || c.Runtime.MaxSessions < 1 || c.Runtime.MaxSessions > 64 || c.Runtime.MaxSessionRecords < c.Runtime.MaxSessions || c.Runtime.MaxSessionRecords > 1024 {
		return fail("runtime session capacity")
	}
	if len(c.Models) == 0 || len(c.Models) > 100 {
		return fail("models")
	}
	seen := make(map[string]bool)
	for i, m := range c.Models {
		field := fmt.Sprintf("models[%d]", i)
		if !identifier(m.ID) || seen[m.ID] {
			return fail(field + ".id")
		}
		seen[m.ID] = true
		if !plain(m.DisplayName) {
			return fail(field + ".display_name")
		}
		// Pin a concrete model name instead of following moving CLI aliases.
		if !identifier(m.CLIModel) || !strings.HasPrefix(m.CLIModel, "claude-") {
			return fail(field + ".cli_model")
		}
		if m.OutputTokenLimit < 1 || m.OutputTokenLimit > 1<<31-1 {
			return fail(field + ".output_token_limit")
		}
		if m.ContextWindow != nil && (*m.ContextWindow < m.OutputTokenLimit || *m.ContextWindow <= 0) {
			return fail(field + ".context_window")
		}
		efforts := make(map[string]bool)
		for _, effort := range m.Efforts {
			if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, effort) || efforts[effort] {
				return fail(field + ".efforts")
			}
			efforts[effort] = true
		}
	}
	if _, ok := c.Model(c.DefaultModel); !ok {
		return fail("default_model")
	}
	return nil
}

func (c Config) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func identifier(value string) bool {
	return plain(value) && len(value) <= 128 && !strings.ContainsAny(value, `/\`) && !strings.ContainsFunc(value, unicode.IsSpace)
}

func profileName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}

func plain(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}
