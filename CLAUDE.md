# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Prism is a local-first CLI that reviews code changes (unstaged/staged/commit/range/snippet) using LLM providers and emits findings in human-readable and machine-readable formats with deterministic exit codes for CI gating. It is **not** an MCP server, GitHub App, formatter, or linter replacement.

The full specification lives in `specs/SPEC.md`.

## Build & Test Commands

```bash
go build ./cmd/prism          # Build the binary
go test ./...                  # Run all tests
go test -v ./internal/gitctx  # Run tests for a single package
go test -run TestFuncName ./internal/review  # Run a single test
```

## Architecture

The project follows a diff-centric pipeline:

1. **CLI** (`internal/cli/`) — Cobra-based command parsing. Subcommands: `review` (unstaged/staged/commit/range/snippet), `config` (init/set/show), `models` (list/doctor), `cache` (show/clear), `version`.
2. **Git Context** (`internal/gitctx/`) — Extracts diffs from git for all 5 review modes, applies path include/exclude filters, truncates at max-diff-bytes.
3. **Secret Redaction** (`internal/redact/`) — Regex-based detection of API keys, JWTs, private keys, AWS patterns, GitHub/Slack/Anthropic/OpenAI tokens. Path-based redaction also supported.
4. **Review Engine** (`internal/review/`) — Contains core types (Finding, Report, Severity, etc.), prompt assembly, LLM response JSON parsing, one repair pass on invalid response, stable finding ID generation. Automatically chunks large diffs (>100KB) into per-file chunks reviewed in parallel (bounded concurrency of 4), then merges/deduplicates findings. Includes compare mode (`compare.go`) for multi-model review with fuzzy finding matching, and rules pack support (`rules.go`) for severity overrides, focus areas, and required checks.
5. **Providers** (`internal/providers/`) — Implements `Reviewer` interface for Anthropic, OpenAI, Google/Gemini, and Ollama/LM Studio (local, `ollama` or `lmstudio`). Each with timeouts, retries, and rate-limit backoff via shared `retryWithBackoff`.
6. **Output** (`internal/output/`) — Text, JSON, Markdown (PR-comment-friendly with collapsible sections), and SARIF v2.1.0 formatters. All support `--out` file or stdout.
7. **Config** (`internal/config/`) — Merges config with precedence: CLI flags > env vars > config file > defaults. Supports `config init/set/show`.
8. **Cache** (`internal/cache/`) — File-based cache with SHA-256 hashed keys, TTL expiration, and stats. Stores redacted review responses only. Cache dir: `$XDG_CACHE_HOME/prism`.

Entry point: `cmd/prism/main.go`

### Key Interface

```go
type Reviewer interface {
    Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error)
    Name() string
}
```

### Exit Codes

- `0` — success, no findings at/above fail threshold
- `1` — findings at/above `--fail-on` severity
- `2` — usage error / invalid args
- `3` — provider auth/config error
- `4` — runtime error (git, IO, schema validation failure)

### Finding IDs

IDs are stable hashes of `path + title + hunk context` so CI diffs remain consistent across runs.

## Configuration

Config file location: `$XDG_CONFIG_HOME/prism/config.json`

Environment variables: `PRISM_PROVIDER`, `PRISM_MODEL`, `PRISM_FAIL_ON`, `PRISM_FORMAT`, `PRISM_MAX_FINDINGS`, `PRISM_CONTEXT_LINES`. Provider keys: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`. Ollama reads `OLLAMA_HOST` and the optional `PRISM_OLLAMA_API_KEY`.

## Code Quality

Run a prism review of the branch before you open a pull request:

```bash
prism review range origin/main..HEAD
```

Fix findings of severity **high** before you open the pull request.
Do not run it after every edit. Each run sends the diff to the configured provider, which is usually external.

For security-sensitive changes, use compare mode:

```bash
prism review range origin/main..HEAD --compare openai:gpt-5.2,gemini:gemini-3-flash-preview
```

## Design Principles

- **Lean dependencies**: stdlib + one CLI library, avoid large frameworks
- **Diff-centric**: Review changes only, not entire repos
- **Provider-agnostic**: All LLM interaction goes through the `Reviewer` interface
- **No live API calls in CI tests**: Use mocked HTTP clients for provider tests
- **Mocked HTTP tests**: Provider tests use `httptest.NewServer` with a `rewriteTransport` to redirect API calls to local test servers
- **Cobra CLI**: Uses `github.com/spf13/cobra` — the only external dependency
