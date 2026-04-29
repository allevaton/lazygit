Before doing anything else, read AGENTS.md and follow it.

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Lazygit is a terminal UI for git, written in Go. It shells out to the `git` binary for all git operations and renders the UI on top of [gocui](https://github.com/jesseduffield/gocui) (vendored under `vendor/github.com/jesseduffield/gocui`, with active development on its `awesome` branch).

## Common commands

The project uses `make` and `just` interchangeably (both wrap the same commands):

- Build (debug-friendly, optimizations off): `make build` / `just build` → produces `./lazygit`
- Run: `make run` / `just run`
- Run with debug + log tailing in two terminals: `make run-debug` in one, `make print-log` in the other (or `LOG_LEVEL=warn go run main.go -debug`)
- Unit tests: `make unit-test` (`go test ./... -short`)
- Lint: `make lint` (wraps `./scripts/golangci-lint-shim.sh run`)
- Format: `make format` (uses `gofumpt`, stricter than `gofmt`)
- Regenerate auto-generated files (test list, cheatsheets, JSON schema): `make generate` (`go generate ./...`). Run this whenever you add an integration test, change keybindings, or change the user config struct.

### Integration tests

Integration tests live in `pkg/integration/tests/` and each test must be registered in `pkg/integration/tests/test_list.go` (auto-generated — re-run `go generate ./...` after adding a test file).

- TUI test picker (easiest, lets you select and run a single test interactively): `make integration-test-tui` / `just e2e-tui`
- Run a single test by name from CLI: `go run cmd/integration_test/main.go cli commit/new_branch` (test name is the path under `pkg/integration/tests/` minus `.go`)
- Run a single test slowly to watch it: add `--slow`, or set `INPUT_DELAY=<ms>`
- Sandbox a test (run setup, then drive lazygit yourself): `--sandbox` (or press `s` in the TUI)
- Debug a test: `go run cmd/integration_test/main.go cli -debug <test>` then attach a debugger to process `test_lazygit`
- Run all integration tests headlessly (CI mode): `make integration-test-all` (`go test pkg/integration/clients/*.go`)

The repo created during a test is left at `test/_results/` for inspection on failure.

## Architecture

The full codebase guide is at `docs/dev/Codebase_Guide.md` — read it for anything non-trivial. Key points:

### Layered structure (top to bottom)

- `pkg/gui/controllers/` — controllers pair keybindings with handlers. One controller can attach to multiple contexts (e.g. the list controller is attached to every list-style context). Controllers may call helpers, contexts, and views.
- `pkg/gui/controllers/helpers/` — shared logic extracted out when more than one controller needs it. Controllers cannot call other controllers' methods, so the rule is: keep code in a controller until a second controller needs it, then promote to a helper.
- `pkg/gui/context/` — one context per view (branches, commits, files, etc.). Contexts hold view-specific state and write content to their view. Contexts may only call views.
- `pkg/gui/types/views.go` and `pkg/gui/views.go` — view definitions and z-ordering.
- `vendor/.../gocui` — underlying terminal UI library (event loop, rendering, key dispatch).

Dependency direction is one-way: controllers → helpers → contexts → views. Views never know about contexts/controllers.

### Other important packages

- `pkg/commands/git_commands/` — every shell-out to `git` lives here. If you need a new git operation, add a method to the relevant struct in this package, not in a controller.
- `pkg/commands/oscommands/` — generic OS / subprocess invocation.
- `pkg/commands/models/` — model structs (Commit, Branch, File, …) returned from git_commands.
- `pkg/config/user_config.go` — defines the user config struct and defaults. See "UserConfig" below.
- `pkg/i18n/english.go` — every user-facing string. Add a field to `TranslationSet` and set its English value in `EnglishTranslationSet()`. Use sentence case. Translations in `pkg/i18n/translations/` are managed by maintainers via Crowdin — do not edit directly.
- `pkg/app/` — startup; `pkg/app/daemon/` — short-lived helper subprocess used as `GIT_EDITOR` for interactive rebase TODO files (despite the name, not a long-running daemon).
- `pkg/integration/` — the integration test harness; `pkg/integration/components/` is where shared test helpers live.

### Key files when orienting

- `pkg/gui/gui.go` — top-level Gui struct and run loop.
- `pkg/gui/layout.go` — invoked on every render; lays out windows.
- `pkg/gui/controllers/helpers/window_arrangement_helper.go` — window sizes/positions.
- `pkg/gui/context.go` — context stack and focus changes.
- `pkg/gui/keybindings.go` — keybindings not yet migrated to a controller (legacy; new keybindings should go on a controller).
- `pkg/gui/controllers.go` — wires controllers to contexts.
- `pkg/gui/controllers/helpers/refresh_helper.go` — re-loads models from git after an action.

### Concepts

- **Window vs view vs tab**: a *window* is a screen region; multiple views can occupy one window (e.g. Files / Worktrees / Submodules tabs all share the files window — switching tabs brings a different view to the front in the same window). "Panel" is deprecated.
- **Common struct (`self.c`)**: most structs hold a `c` field carrying common deps (logger, i18n, UserConfig, helpers, etc.). To reach a helper from a controller: `self.c.Helpers.MyHelper`.
- **Async work**: `self.c.OnWorker(fn)` to do work off the UI thread; `self.c.OnUIThread(fn)` to bounce back. Don't block the event loop.

### UserConfig reloading

The user config can be hot-reloaded while lazygit is running. Most code accesses it via the `Common` struct, which gets pointer-replaced on reload, so reads are always fresh — no extra work needed. If a config value drives state that must update when reload happens, add handling in `Gui.onUserConfigLoaded`. As a last resort, list it in `Gui.checkForChangedConfigsThatDontAutoReload` (which prompts the user to restart).

## Project-specific conventions

- Use `self` as the receiver name on struct methods (intentional, project-wide).
- Interfaces with multiple methods may be prefixed `I` rather than suffixed `er`.
- When a struct implements an interface, document it explicitly: `var _ MyInterface = &MyStruct{}` near the type. This forces a compile error in the implementing file if the interface drifts.
- Format with `gofumpt`, not `gofmt`.
- `vendor/` is checked in — `go mod vendor && go mod tidy` (also `make vendor`) regenerates it.

## Adding logging during development

From most code: `self.c.Log.Warn(...)` or `gui.Log.Warn(...)`.
From inside `vendor/` (e.g. patching gocui locally): there's no injected logger, but you can use the global `logs.Global.Warn(...)` and set `LAZYGIT_LOG_PATH=/some/file` to capture output.

## Contributing

Per `CONTRIBUTING.md`, the upstream project does not currently accept unsolicited PRs. Local development of patches and forks is fine; just be aware that opening a PR against the upstream repo is unlikely to be reviewed unless coordinated via an issue first.
