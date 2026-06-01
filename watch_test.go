package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The watcher rides real FSEvents, which have inherent latency and coalescing,
// so the timings here are deliberately generous: settle before mutating, wait
// up to recvTimeout for a broadcast.
const (
	settleDelay = 800 * time.Millisecond
	recvTimeout = 5 * time.Second
	quietWindow = 1500 * time.Millisecond
)

// TestWatchBroadcastsNewUntrackedFile: creating a file the change set didn't
// have must broadcast (status output gains the untracked entry).
func TestWatchBroadcastsNewUntrackedFile(t *testing.T) {
	repo, st, h, ch := startWatched(t)
	defer h.remove(ch)

	write(t, filepath.Join(repo, "added.txt"), "hi\n")

	id, ok := recv(ch, recvTimeout)
	if !ok {
		t.Fatal("no broadcast after creating an untracked file")
	}
	if want := st.repos()[0].ID; id != want {
		t.Fatalf("broadcast id = %q, want %q", id, want)
	}
}

// TestWatchBroadcastsRepeatedContentEdit is the regression guard for the bug
// this fixed: the FIRST edit (clean -> modified) always broadcast, but a SECOND
// content edit kept the same status letter, so the old name-status signature
// was identical and the open diff never refreshed. The signature now folds in
// the full patch, so both edits broadcast.
func TestWatchBroadcastsRepeatedContentEdit(t *testing.T) {
	repo, _, h, ch := startWatched(t)
	defer h.remove(ch)

	tracked := filepath.Join(repo, "README") // committed by mustGitRepo

	write(t, tracked, "first edit\n") // clean -> M
	if _, ok := recv(ch, recvTimeout); !ok {
		t.Fatal("first content edit (clean -> modified) did not broadcast")
	}
	drain(ch)

	write(t, tracked, "second edit\n") // M -> M, content differs
	if _, ok := recv(ch, recvTimeout); !ok {
		t.Fatal("second content edit (same status, new content) did not broadcast")
	}
}

// TestWatchSkipsIgnoredDirChurn: writes under a skipWatch segment must not
// broadcast, so build/dep churn can't storm clients.
func TestWatchSkipsIgnoredDirChurn(t *testing.T) {
	repo, _, h, ch := startWatched(t)
	defer h.remove(ch)

	// Pre-create the dir before the watch is primed so the only events we
	// generate are the writes *inside* it (those carry the skipped segment).
	nm := filepath.Join(repo, "node_modules")
	mkdir(t, nm)
	time.Sleep(settleDelay)
	drain(ch)

	write(t, filepath.Join(nm, "pkg.js"), "noise\n")

	if _, ok := recv(ch, quietWindow); ok {
		t.Fatal("write under node_modules should be skipped, but broadcast")
	}
}

// TestWatchDiscoversNewRepo: a repo created under a root *after* the watcher
// started must be discovered and broadcast, not stay invisible until restart.
// Discovery is one-shot at boot, so this fails until the watcher rescans when a
// .git entry appears outside every known repo.
func TestWatchDiscoversNewRepo(t *testing.T) {
	repo, st, h, ch := startWatched(t)
	defer h.remove(ch)
	root := filepath.Dir(repo)

	// Init a brand-new repo next to the existing one, with an untracked file so
	// it has something to show.
	fresh := filepath.Join(root, "fresh")
	mustGitRepo(t, fresh)
	write(t, filepath.Join(fresh, "new.txt"), "hi\n")

	want := idFor(fresh)
	if !recvMatch(ch, want, recvTimeout) {
		t.Fatal("new repo under the root was not discovered/broadcast")
	}
	if _, ok := st.get(want); !ok {
		t.Fatalf("new repo %q absent from store after discovery", want)
	}
}

// recvMatch waits up to total for a broadcast carrying want, tolerating (and
// discarding) broadcasts for other repos in between.
func recvMatch(ch chan string, want string, total time.Duration) bool {
	deadline := time.Now().Add(total)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		id, ok := recv(ch, remaining)
		if !ok {
			return false
		}
		if id == want {
			return true
		}
	}
}

// startWatched builds a one-repo store under a temp root, starts the watcher,
// subscribes to the hub, and waits for the FSEvents watch to attach and the
// baseline signature to be primed. Cancellation stops the watcher on cleanup.
func startWatched(t *testing.T) (repo string, st *store, h *hub, ch chan string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	mustGitRepo(t, repo)

	st = newStore([]string{root})
	if n := len(st.repos()); n != 1 {
		t.Fatalf("discoverRepos: got %d repos, want 1", n)
	}

	h = newHub()
	ch = h.add()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go watch(ctx, st, h, 50*time.Millisecond)
	time.Sleep(settleDelay)
	return repo, st, h, ch
}

func recv(ch chan string, d time.Duration) (string, bool) {
	select {
	case id := <-ch:
		return id, true
	case <-time.After(d):
		return "", false
	}
}

func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func mustGitRepo(t *testing.T, dir string) {
	t.Helper()
	mkdir(t, dir)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "gitknown-test"},
		{"config", "commit.gpgsign", "false"},
	} {
		runGit(t, dir, args...)
	}
	write(t, filepath.Join(dir, "README"), "initial\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
}
