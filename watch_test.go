package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// TestWatchDropsRemovedRepo: a repo/worktree removed from disk while running
// must be dropped from the store, not linger as a phantom (stale count, empty
// tree). Its own deletion events map back to it via repoFor, so removal never
// trips the .git rescan gate; the watcher must detect the vanished path itself.
func TestWatchDropsRemovedRepo(t *testing.T) {
	repo, st, h, ch := startWatched(t)
	defer h.remove(ch)
	root := filepath.Dir(repo)

	// A second repo under the same root, with a change so it's dirty.
	extra := filepath.Join(root, "extra")
	mustGitRepo(t, extra)
	write(t, filepath.Join(extra, "new.txt"), "hi\n")
	want := idFor(extra)
	if !recvMatch(ch, want, recvTimeout) {
		t.Fatal("second repo was not discovered/broadcast")
	}
	if _, ok := st.get(want); !ok {
		t.Fatalf("repo %q absent from store before removal", want)
	}

	// Remove it from disk; the watcher must drop it from the store. Removal is
	// eventually-consistent (a mid-deletion change can broadcast before the dir
	// is fully gone), so poll the store rather than asserting on one broadcast.
	if err := os.RemoveAll(extra); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(recvTimeout)
	for {
		if _, ok := st.get(want); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("removed repo %q still in store (phantom)", want)
		}
		recv(ch, 100*time.Millisecond) // drain broadcasts while waiting
	}
}

// TestWatchPollRediscoversRepo is the regression guard for the bug this fixed:
// a repo dropped out of the store while running (its base was momentarily
// unresolvable during a git op, so discoverRepos skipped it on a rescan and the
// store replaced its list without it) never came back. The only other rescan
// trigger is a .git write outside every known repo, which an idle tree never
// makes, so the repo lingered missing until a manual restart. The fallback poll
// now re-runs discovery, so any repo present on disk is picked back up on its
// own. Debounce is pushed past the test's life so the poll is the only timer
// that can rediscover: a repo created after start (absent from the store, its
// creation events never processed) standing in for one that dropped out.
func TestWatchPollRediscoversRepo(t *testing.T) {
	repo, st, h, ch := startWatchedCfg(t, watchConfig{debounce: time.Hour, poll: 100 * time.Millisecond})
	defer h.remove(ch)
	root := filepath.Dir(repo)

	fresh := filepath.Join(root, "fresh")
	mustGitRepo(t, fresh)
	write(t, filepath.Join(fresh, "new.txt"), "hi\n")

	want := idFor(fresh)
	if !recvMatch(ch, want, recvTimeout) {
		t.Fatal("repo on disk but absent from the store was not rediscovered by the poll")
	}
	if _, ok := st.get(want); !ok {
		t.Fatalf("rediscovered repo %q absent from store", want)
	}
}

// TestWatchFallbackPollBroadcasts: the fallback poll re-fingerprints repos on
// its own timer, so a change surfaces even if FSEvents never delivers it. We
// can't suppress FSEvents in-process, so the debounce window is pushed past the
// test's lifetime: the only timer that can fire is the poll, so a broadcast here
// proves the poll path stands on its own.
func TestWatchFallbackPollBroadcasts(t *testing.T) {
	repo, st, h, ch := startWatchedCfg(t, watchConfig{debounce: time.Hour, poll: 100 * time.Millisecond})
	defer h.remove(ch)

	write(t, filepath.Join(repo, "added.txt"), "hi\n")

	id, ok := recv(ch, recvTimeout)
	if !ok {
		t.Fatal("no broadcast from the fallback poll")
	}
	if want := st.repos()[0].ID; id != want {
		t.Fatalf("poll broadcast id = %q, want %q", id, want)
	}
}

// TestWatchHeartbeatLogs: with a heartbeat interval set, the watcher must log a
// liveness line on its own, so a silently-stalled stream is diagnosable from the
// logs. Debounce is pushed out so the heartbeat is the only timer in play.
func TestWatchHeartbeatLogs(t *testing.T) {
	var buf lockedBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	startWatchedCfg(t, watchConfig{debounce: time.Hour, heartbeat: 100 * time.Millisecond})

	deadline := time.Now().Add(recvTimeout)
	for !strings.Contains(buf.String(), "watcher alive") {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat logged; got:\n%s", buf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lockedBuffer is a bytes.Buffer safe for the watcher goroutine to write while
// the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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
	// Event path only: heartbeat/poll off so these tests exercise FSEvents, not
	// the backstops (a poll would otherwise mask a broken event path).
	return startWatchedCfg(t, watchConfig{debounce: 50 * time.Millisecond})
}

func startWatchedCfg(t *testing.T, cfg watchConfig) (repo string, st *store, h *hub, ch chan string) {
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
	go watch(ctx, st, h, cfg)
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
