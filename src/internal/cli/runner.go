// Package cli adapts the official CLI's native stream-json boundary.
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/logging"
	"unbundle-go/src/internal/messages"
)

// Error never contains raw CLI diagnostics, arguments, paths or credentials.
type Error struct {
	Code      string         `json:"code"`
	Type      string         `json:"type"`
	Message   string         `json:"message"`
	Stage     string         `json:"stage"`
	Retryable bool           `json:"retryable"`
	Outcome   string         `json:"outcome"`
	Usage     messages.Usage `json:"usage"`
	Status    int            `json:"-"`
	location  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func Failure(code, stage, message string, status int) *Error {
	return &Error{Code: code, Type: "api_error", Message: message, Stage: stage, Status: status, Outcome: "unknown"}
}

type Event struct {
	Type string
	Data any
}

type Runner struct{ Config config.Config }

// ValidateTurn rejects capabilities the native adapter cannot apply faithfully.
// No historical roles are flattened into text or imported into a new CLI.
func ValidateTurn(r messages.Request) error {
	unsupported := func(field string) error {
		return &messages.Error{Code: "UNSUPPORTED_PARAMETER", Field: field, Message: "not executable in the native adapter"}
	}
	for _, m := range r.Messages {
		if m.OutputConfig != nil {
			return unsupported("messages.output_config: per-message effort changes")
		}
	}
	return nil
}

// environment preserves the official CLI's login location, not gateway or
// caller credentials. Routing, provider, agent and plugin overrides are excluded.
func environment() []string {
	var env []string
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "CLAUDE_CONFIG_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "DISABLE_PLUGIN_AUTOLOAD=1", "CLAUDE_CODE_SKIP_PLUGIN_MCP_SERVERS=1", "DISABLE_AUTOUPDATER=1", "DISABLE_AUTO_COMPACT=1", "CLAUDE_CODE_MAX_RETRIES=0", "CLAUDE_CODE_TOTAL_TOKENS_REMINDER=off", "ENABLE_TOOL_SEARCH=false")
}

func command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = environment()
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	return cmd
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errors.New("CLI metadata exceeds limit")
	}
	return b.Buffer.Write(p)
}

// CheckVersion is model-free. A matching version is not protocol acceptance or
// proof of account access; live acceptance is a separate explicit operation.
func (r Runner) CheckVersion(ctx context.Context) error {
	logger := logging.From(ctx)
	logger.Debug("cli_version_check_started", "expected_version", r.Config.CLI.Version)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := command(ctx, r.Config.CLI.Path, "--version")
	var output boundedBuffer
	var diagnostics diagnosticCount
	cmd.Stdout = &output
	cmd.Stderr = &diagnostics
	if err := cmd.Run(); err != nil {
		logger.Error("cli_version_check_failed", logging.Cause(err), "stderr_bytes", diagnostics.bytes.Load())
		return Failure("CLI_VERSION_FAILED", "startup", "could not check the official CLI version", 503)
	}
	if strings.TrimSpace(output.String()) != r.Config.CLI.Version+" (Claude Code)" {
		logger.Error("cli_version_mismatch", "expected_version", r.Config.CLI.Version)
		return Failure("CLI_VERSION_MISMATCH", "startup", "official CLI version does not match configuration", 503)
	}
	logger.Debug("cli_version_check_finished", "version", r.Config.CLI.Version)
	return nil
}
