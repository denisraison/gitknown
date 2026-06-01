# gitknown

One web page that aggregates uncommitted + unpushed git changes across every
repo and worktree under a set of roots, so you can review work-in-progress
(e.g. what Claude Code is doing across many checkouts) on the fly, before any
of it becomes a PR.

Single Go binary: it embeds the built frontend and serves the API from the same
process. Bind to localhost only.

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

Per repo it diffs against the **merge-base** (fork point) of HEAD and the
branch's upstream — or `origin/main`/`master` when there's no upstream. That
means you see everything *this* branch added (unpushed commits + staged +
unstaged + untracked) and never changes the base advanced past you.

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
- `web/` — Solid + TypeScript frontend wrapping the two Pierre vanilla cores

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
