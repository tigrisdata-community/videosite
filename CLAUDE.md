# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

`task` and `templ` are Go tools (`go.mod` `tool` block) — invoke through `go tool task <name>`. Definitions live in `taskfile.yml`:

- `go tool task` (alias for `task run`) — regenerates templ + runs `go run ./cmd/videosite`.
- `go tool task generate` — regenerates templ; required before `go build`. Other tasks that need it depend on it already.
- `go tool task test:all` — full suite, `go test -race ./...` (matches CI).
- `go tool task test -- <pkg> [flags]` — single-package run, e.g. `go tool task test -- ./internal/encoder -run TestName`.
- `go tool task docker` — builds and pushes both images via buildx bake.
- `go tool task db` — opens a `sqlite3` REPL against `./var/data.db`.

Config is via flags or matching env vars (`facebookgo/flagenv`), with `.env` auto-loaded. See `cmd/videosite/main.go` for the full set.

For commits, use `/conventional-commits`.

### AI attribution

Every commit must include both trailers in the footer:

```text
Assisted-by: <Model Name> via Claude Code
```

Disclose the assisting model in `Assisted-by` per the conventional-commits skill (e.g. `Assisted-by: Claude Opus 4.7 via Claude Code`). Commit with `git commit --signoff` so the sign-off trailer is added automatically.

## Architecture

Videosite is a simple CRUD app built with Go and GORM.

- Use the **gorm-dao** skill when operating with the database.
- Use the **go-table-driven-tests** skill when writing test code.
- Use the **xe-go-style** skill when writing Go code.

When doing multi-step operations where each step depends on the results of a previous step, model this as a state machine. See how video uploads work in `internal/models` — every transition goes through a helper that requires a specific `from` status and returns `ErrConflict` when the row isn't in that state, which is how the orchestrator tick, the webhook, and the upload finalize path stay coordinated.

HTML generation is done with [templ](https://templ.guide). Use these skills when dealing with templ:

- **templ-syntax** — IMPORTANT, load this one first.
- **templ-components**
- **templ-htmx**
- **templ-http**

Templ files are under `web/*.templ`; run `go generate ./...` after editing to regenerate the `*_templ.go` files.

### Plans

Background lives in `docs/plans/`. They're design docs, not specifications — code is authoritative when they disagree.
