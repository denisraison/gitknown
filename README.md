# gitknown

[![CI](https://github.com/denisraison/gitknown/actions/workflows/ci.yml/badge.svg)](https://github.com/denisraison/gitknown/actions/workflows/ci.yml)

## Why

You end up with a lot of worktrees. Agents (Claude Code and friends) churn
across many checkouts at once, and `git status` one-repo-at-a-time doesn't
scale: you lose track of which tree has uncommitted edits, which has unpushed
commits, and what any of it actually changed before it turns into a PR.

gitknown is one local web page that aggregates the **uncommitted + unpushed**
work across every repo and worktree under a set of roots, live, so you can see
and review all of it in one place. It's read-only and binds to localhost: a
review surface, not a git client.

Single Go binary: it embeds the built frontend and serves the API from the same
process.

## What it shows

- Left rail: every dirty repo/worktree with a changed-file count, an unpushed
  commit marker (`↑N`), and its `branch · base` label.
- Middle: a [`@pierre/trees`](https://trees.software) file tree of that repo's
  changes, with git-status badges. A toggle switches it to **all files** (the
  whole repo, gitignore-respected) so you can open unchanged files for context;
  changed files stay badged, unchanged ones open read-only.
- Right: a [`@pierre/diffs`](https://diffs.com) split diff of the selected file.
- Live: a recursive filesystem watcher (FSEvents on macOS, via
  [`rjeczalik/notify`](https://github.com/rjeczalik/notify)) pushes Server-Sent
  Events the instant a repo changes on disk, so counts and the open diff refresh
  without a reload. A repo cloned/created under a root while running is
  discovered live (a new `.git` entry triggers a rescan).

## Change scope

The point is to show *your* in-flight work, not your branch's whole history.
So per repo:

- **Branch with an upstream** → diff against the **merge-base** (fork point) of
  HEAD and that upstream. You see everything this branch added that the remote
  doesn't have (unpushed commits + staged + unstaged + untracked), and the base
  never moves when the remote advances past you.
- **No resolvable upstream** (never pushed, or its remote-tracking ref isn't
  present locally) → base is **HEAD**, so only uncommitted work shows; committed
  work stays hidden until the branch has somewhere to push.

It deliberately never falls back to `origin/main`. Diffing a feature branch
against a different branch surfaces its entire committed history as "changed"
forever, which is exactly the noise we're trying to cut.

## Install

Each release publishes prebuilt, tagged binaries (linux-amd64, darwin-arm64),
each with a `.sha256`, on the [releases page](https://github.com/denisraison/gitknown/releases).
With nix you can run the latest published release directly, no clone or build:

```sh
nix run github:denisraison/gitknown -- --roots ~/work
```

That pulls the prebuilt release binary. To build from source instead (e.g. an
unreleased commit), use `nix build github:denisraison/gitknown#gitknown`.

## Setup

Toolchain is pinned via a nix flake (`go`, `nodejs`, `just`, `golangci-lint`,
tracking `nixpkgs-unstable`). With direnv:

```sh
direnv allow   # loads the flake dev shell on cd
```

Or without direnv: `nix develop`.

## Run

```sh
just deps           # one-time: install frontend deps
just build          # build frontend, then the embedding binary
just run            # build + run against $ROOTS (default ~/workspace)
# open http://127.0.0.1:8484

ROOTS=~/work ADDR=127.0.0.1:9000 just run   # override roots/addr
```

Binary flags: `--roots` (comma-separated dirs), `--addr`, `--web` (serve
frontend from a dir instead of the embedded build), `--debounce` (coalesce
window for filesystem change events, default 200ms).

Deep link to a specific diff: `/?repo=<id>&file=<path>` (or `repo=first&file=first`).

## Dev

```sh
just dev   # Go backend + Vite dev server, both hot-reloading; open the Vite URL
```

Both sides hot-reload: [`wgo`](https://github.com/bokwoon95/wgo) rebuilds and
restarts the Go backend on any `.go` change, and Vite hot-reloads the frontend
(proxying `/api` to the backend). The backend comes up first (dev waits for the
API to listen before starting Vite), and Ctrl-C tears down both with no orphaned
processes left holding the port.

## Lint, verify, hooks

The frontend builds with Vite 8 (Rolldown), lints with [`oxlint`](https://oxc.rs)
and formats with [`oxfmt`](https://oxc.rs); the backend lints with a strict
`golangci-lint` (`.golangci.yml`) that forbids unchecked **and** blank-discarded
errors.

```sh
just lint            # golangci-lint + (oxlint + oxfmt --check + tsc)
just fix             # gofmt + oxlint --fix + oxfmt (auto-fix)
just verify          # full gate: lint + go test + both builds
just install-hooks   # wire hooks manually (normally automatic, see below)
```

Git hooks are managed by [lefthook](https://lefthook.dev) (`lefthook.yml`),
split so commits stay fast:

- **pre-commit** runs `golangci-lint`, `oxlint`, `oxfmt --check`, and `tsc` in
  parallel, scoped to the staged files (~1s).
- **pre-push** runs the full `just verify` (lint + tests + both builds).

Hooks install themselves: `just deps` (`npm install`) runs a `prepare` script
that calls `lefthook install`, so a fresh clone wires the hooks as part of the
deps step you already run. `just install-hooks` does the same explicitly.
Commit/push from inside the dev shell (direnv loads it) so the tools are on
`PATH`; bypass once with `git commit --no-verify` or `LEFTHOOK=0 git push`.

## Layout

- `git.go` — repo/worktree discovery, base resolution, status, diff
- `server.go` — JSON + SSE handlers, repo store, event hub
- `main.go` — flags, embedded static serving, filesystem watcher
- `update.go` — self-managed install detection + GitHub-release autoupdater
- `web/` — Solid + TypeScript frontend wrapping the two Pierre vanilla cores

See [ARCHITECTURE.md](ARCHITECTURE.md) for how the pieces fit together (the data
flow, the watcher, the change fingerprint, the self-updater).

## Known rough edges (v1)

- Watcher change-detection is content-aware for *tracked* files (the signature
  folds in the full `git diff`), so editing a file already in the change set
  refreshes the open diff. The remaining gap: re-editing an *untracked* file's
  contents won't refresh (untracked content isn't in the diff, and hashing every
  untracked file would be costly on huge build dirs). The seam is
  `statusSignature()` in `git.go`.
- No cap on giant change sets: a repo with a huge untracked build/data dir
  (e.g. a `*-demo` with 17k untracked files) lists them all. Cap/flag in the UI.
- Read-only. No staging/committing/accept-reject from the web.
- `git show <base>:<path>` is shelled per file; fine locally, not optimized.

## License

MIT. See [LICENSE](LICENSE).
