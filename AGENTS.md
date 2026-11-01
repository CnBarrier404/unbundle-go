# AGENTS.md

unbundle-go is a Go rewrite of Unbundle, an experimental, self-hosted CLI adapter for a single user. Its purpose is to separate cloud inference from local tool execution and approvals: the official Claude Code CLI provides inference on a server, an SSH tunnel connects the gateway to the local client, and the workspace, tools, and approvals remain under the local client's control.

## Commands

| Action | Command                                                  |
| :----- | :------------------------------------------------------- |
| Build  | `go build -o bin/ ./src/cmd/unbundle`                    |
| Format | `go fmt ./...` and `npx --no-install prettier --write .` |
| Test   | `go test ./...`                                          |
| Vet    | `go vet ./...`                                           |

## Rules

- Avoid reinventing the wheel.

  Before implementing functionality from scratch, check whether established, well-maintained libraries, packages, or open-source solutions already address the requirement. Prefer adopting suitable existing solutions over building custom implementations. Only implement a custom solution when existing options are unsuitable, introduce disproportionate complexity, or fail to meet the project's constraints.

- Do not add unsolicited tests.

  Do not add tests unless the user explicitly requests them. Existing tests may be run to verify changes.

- Keep changes minimal.

  Implement only what is necessary to satisfy the request. Avoid unnecessary abstractions, speculative features, and unrelated refactoring.

- Remove obsolete implementations.

  When replacing an implementation, remove the old implementation. Do not retain backward-compatibility code or legacy entry points unless the user explicitly requests them.

### Development

- Keep Go source under `src/`, entry points under `src/cmd/`, and private packages under `src/internal/`.
- Before completing changes, run the Format commands above or `make fmt`. Use Prettier defaults.

### Architecture

- Use the official Claude Code CLI for server-side inference. Keep workspaces, tool execution, and approvals local.
- Target Claude Code desktop, the Code workspace in Claude Desktop, for daily inference.
- Support one VPS, one account, and one fixed local machine, with concurrent sessions.
- Keep ordinary diagnostics model-free; require explicit activation for live inference.

### Deployment

- Bind the gateway to loopback and access it through SSH. Reuse existing SSH configuration and host verification.
- Leave OAuth login, credential storage, and token refresh to the official CLI. Keep private keys and gateway tokens out of logs and Git.
- Preserve existing deployments and client configurations; use separate profiles and available ports for new deployments.
