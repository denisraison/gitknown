# Architecture

How gitknown works. For *why* it exists and how to run it, see the
[README](README.md); this doc is the mechanics.

It's a single Go process. The backend discovers git working trees under the
scan roots, watches them for changes, and serves both a JSON/SSE API and the
embedded frontend. The frontend is a Solid SPA built by Vite and baked into the
binary with `//go:embed`, so there's nothing to deploy alongside it.

## Data flow

```
roots ──► discoverRepos ──► store ──────────────► GET /api/repos      (left rail)
            (git.go)        (server.go)           GET /api/repos/{id}/files|tree|file
                              ▲                          (loadRepo / changedFiles / fileDiff)
                              │ refreshOne
                              │
FSEvents ──► watch ──► statusSignature changed? ──► hub.broadcast(id) ──► SSE: event: changed
(notify)    (main.go)   (fnv1a of status+diff)       (server.go)          GET /api/events
```

A change on disk flows: kernel FSEvents → `notify` channel → debounce → map the
path back to a repo → re-fingerprint → if the signature moved, refresh that repo
in the store and broadcast its id → every connected browser gets an SSE
`changed` event and refetches that repo (and the open diff if it's the one
showing).

## Backend pieces

### Discovery — `git.go: discoverRepos`

`filepath.WalkDir` over each root. A directory is a working tree if it has a
`.git` entry — and a *linked worktree*'s `.git` is a file (pointing into the
main repo's `.git/worktrees`), not a dir, so statting any `.git` entry catches
worktrees too. On a hit we record the tree and stop descending (nested
worktrees live in sibling dirs, not inside the tree). `dirsToSkip`
(`node_modules`, `vendor`, `dist`, `target`, `.venv`, `.next`, `.git`) keeps the
walk cheap. Repo identity is `fnv1a(path)` — stable across rescans.

### Base + change set — `git.go: resolveBase, changedFiles`

`resolveBase` picks the ref to diff against (see [README → Change
scope](README.md#change-scope) for the rationale): upstream's merge-base with
HEAD if there is one, else HEAD, never `origin/main`. `changedFiles` is then
`git diff --name-status -M <base>` (tracked changes, incl. unpushed commits)
unioned with `git ls-files --others --exclude-standard` (untracked,
gitignore-respected). `Ahead` is `rev-list --count <base>..HEAD`.

### Change fingerprint — `git.go: statusSignature`

The watcher needs to know *whether anything that matters changed* without
re-broadcasting on noise. `statusSignature` is an FNV-1a hash folding in both
`status --porcelain=v2 --branch` **and** the full `git diff <base>`. The diff
matters: porcelain only carries paths and status letters, so editing the
*contents* of a file already marked `M` would otherwise hash identically and the
open diff would never refresh. Both git calls use `--no-optional-locks` so
fingerprinting can't collide with the user's own git commands on `index.lock`.

### Watcher — `main.go: watch`

One recursive FSEvents watch per root via
[`rjeczalik/notify`](https://github.com/rjeczalik/notify) (kernel-coalesced, no
per-directory descriptors). Event paths are matched back to a repo by
longest-prefix (most specific worktree wins), against canonicalized realpaths
because FSEvents always reports the realpath. Mechanics:

- **Debounce.** Events accumulate into a `pending` set; a ticker drains it every
  `--debounce` (200ms default), re-fingerprinting each touched repo once.
- **`.git` is watched** (not skipped), so commits and pushes update the `↑N`
  ahead marker. `node_modules`/`.venv`/`.next`/`.direnv` churn is skipped.
- **Live discovery.** A `.git` entry appearing under a root but outside every
  known repo flags a rescan, so a repo cloned or a worktree added while running
  shows up without a restart. New repos are primed silently, then announced.
- **Gone detection.** A repo whose working tree has vanished (worktree removed)
  is dropped from the store and broadcast so the UI stops showing a phantom.
- **Two backstops** for the failure mode where FSEvents silently stops
  delivering with the process still alive (sleep, a wedged stream): a
  *heartbeat* logs liveness + event/broadcast counts so a stall is diagnosable,
  and a *fallback poll* (`--poll`, 1min) re-fingerprints every repo on its own
  timer so changes still surface at poll latency regardless.

### Store + hub — `server.go`

`store` is the in-memory repo cache (RWMutex, `byID` map + ordered `list`);
`refresh` rebuilds it from discovery, `refreshOne` re-reads a single repo in
place after a change. `hub` is the SSE fan-out: each client is a buffered
channel, and `broadcast` does a non-blocking send (a slow client drops the
event rather than stalling the watcher).

### HTTP / SSE API — `server.go`

Go 1.22 method-pattern routes, all read-only:

| Route | Returns |
| --- | --- |
| `GET /api/repos` | every discovered repo summary (left rail) |
| `GET /api/repos/{id}/files` | the repo's changed-file list vs its base |
| `GET /api/repos/{id}/tree` | full file listing (all-files mode), `treeCap`-bounded |
| `GET /api/repos/{id}/file?path=&status=` | one file's base/working diff pair |
| `GET /api/repos/{id}/file?path=&mode=view` | unchanged file contents as a no-op diff |
| `GET /api/events` | SSE stream of changed repo ids |

`fileDiff`/`viewFile` anchor the requested path under the repo root
(`filepath.Clean("/"+rel)`) so a crafted `../` can't read or `git show` outside
the working tree.

### Self-update — `update.go`

Claude-Code-style: on startup (and every 6h) the binary checks GitHub releases
and, if newer, stages the new binary for the next restart. It's deliberately
inert unless it's a *self-managed* install — the running executable lives under
`~/.local/share/gitknown/bin/` (resolved via `EvalSymlinks`) — so nix and dev
builds are never clobbered; dev/`-dirty`/pre-release tags are also skipped, and
`GITKNOWN_NO_AUTOUPDATE` opts out. Staging is download → **verify sha256**
against the release's `.sha256` sidecar → extract (with an `io.LimitReader` bomb
guard) → `-version` sanity-check the new binary → atomically swap the `current`
symlink (`.current.tmp` + rename). The agent picks it up on its next start.

## Frontend — `web/`

Solid + TypeScript, built by [Vite 8 (Rolldown)](https://vite.dev). It's a thin
reactive shell around two vanilla [Pierre](https://pierre.co) cores:
[`@pierre/trees`](https://trees.software) for the file tree and
[`@pierre/diffs`](https://diffs.com) for the split diff. It opens one
`EventSource` on `/api/events`; a `changed` event refetches that repo's summary
and, when it's the selected one, its files and the open diff. Deep links
(`/?repo=<id>&file=<path>`, or `repo=first&file=first`) are resolved on load.
In production the binary serves the embedded build; in dev Vite serves the
frontend and proxies `/api` to the Go backend.

## Build + tooling

The toolchain is pinned by a nix flake (`go`, `nodejs`, `just`, `golangci-lint`,
tracking `nixpkgs-unstable`); `just build` builds the frontend then the
embedding binary, stamping `main.version` from `git describe --tags`. Releases
publish per-platform tarballs (`darwin-arm64`, `linux-amd64`) plus `.sha256`
sidecars, which both the self-updater and the nix `gitknown-bin` output consume.
Quality gates: `oxlint`/`oxfmt`/`tsc` on the frontend, a strict `golangci-lint`
on the backend, wired through [lefthook](https://lefthook.dev) (fast staged-file
checks pre-commit, full `just verify` pre-push). See the README for the commands.
