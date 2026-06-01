# gitknown tasks. Run `just` to list.

roots := env_var_or_default("ROOTS", env_var("HOME") / "workspace")
addr := env_var_or_default("ADDR", "127.0.0.1:8484")

# List recipes.
default:
    @just --list

# Install frontend deps (one-time / after package.json changes).
deps:
    cd web && npm install

# Build the frontend, then the single embedding binary.
build:
    cd web && npm run build
    go build -o gitknown .

# Build, then run against {{roots}} on {{addr}}.
run: build
    ./gitknown --roots {{roots}} --addr {{addr}}

# Dev: Go backend + Vite dev server, both hot-reloading. wgo rebuilds+restarts
# the backend on any .go change; Vite hot-reloads the frontend and proxies /api
# to the backend. The API comes up first (we wait for it) so the FE's first
# proxied call can't race a not-yet-listening backend. Ctrl-C tears down both
# with no orphaned processes left holding the port.
dev:
    #!/usr/bin/env bash
    set -euo pipefail
    set -m  # job control: each background job is its own process group, so we
            # can signal the whole tree (node + its workers) by group on exit.

    be_pid="" fe_pid=""
    cleanup() {
        trap - EXIT INT TERM
        # wgo reaps its own backend (go run + the compiled binary) when signalled;
        # the negative-PID kill takes the FE group (node + esbuild/rolldown workers).
        [ -n "$be_pid" ] && kill -TERM "$be_pid" 2>/dev/null || true
        [ -n "$fe_pid" ] && kill -TERM -"$fe_pid" 2>/dev/null || true
        wait 2>/dev/null || true
    }
    trap cleanup EXIT INT TERM

    # Backend first, with hot reload. -file limits restarts to .go edits; -xdir
    # web keeps frontend churn (and node_modules) from restarting the backend.
    wgo -file '\.go$' -xdir web go run . --roots {{roots}} --addr {{addr}} &
    be_pid=$!

    # Block until the API actually accepts connections before starting the FE.
    printf 'dev: waiting for API on %s ' '{{addr}}'
    for _ in $(seq 1 100); do
        if curl -sf -o /dev/null "http://{{addr}}/api/repos"; then
            echo "up"
            break
        fi
        printf '.'
        sleep 0.2
    done

    # Frontend dev server (hot reload).
    ( cd web && npm run dev ) &
    fe_pid=$!

    wait

# --- Lint / test / verify ---------------------------------------------------

# Strict lint, backend + frontend (golangci-lint + oxlint + oxfmt --check + tsc).
lint:
    golangci-lint run ./...
    cd web && npm run check

# All tests, backend + frontend (frontend has none yet).
test:
    go test ./...

# Auto-fix what can be fixed: gofmt (backend) + oxlint --fix + oxfmt (frontend).
fix:
    gofmt -w *.go
    cd web && npm run fix

# Full gate before pushing: lint + tests + both builds (pre-push runs this).
verify: lint test build
    @echo "verify: all checks passed"

# Install lefthook-managed git hooks (see lefthook.yml). One-time per clone.
install-hooks:
    -git config --unset core.hooksPath
    lefthook install
    @echo "hooks installed: pre-commit (lint/fmt/typecheck) + pre-push (verify)"

# Remove build artifacts.
clean:
    rm -f gitknown
    rm -rf web/dist
