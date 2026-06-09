package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rjeczalik/notify"
)

//go:embed all:web/dist
var embedded embed.FS

// version is stamped at build time via -ldflags "-X main.version=...". `just
// build` sets it from `git describe --tags`; the default marks an unstamped
// (e.g. plain `go build`) binary.
var version = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:8484", "listen address (localhost only by default)")
	rootsArg := flag.String("roots", "", "comma-separated dirs to scan (default: cwd)")
	webDir := flag.String("web", "", "serve frontend from this dir instead of the embedded build (dev)")
	debounce := flag.Duration("debounce", 200*time.Millisecond, "coalesce window for filesystem change events")
	heartbeat := flag.Duration("heartbeat", time.Minute, "watcher liveness log interval (0 disables)")
	poll := flag.Duration("poll", time.Minute, "fallback re-scan interval, a safety net for when FSEvents goes silent (0 disables)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// Keep a self-managed install current with GitHub releases, applied on the
	// next start. Inert for nix/dev builds and opt-out via GITKNOWN_NO_AUTOUPDATE.
	if os.Getenv("GITKNOWN_NO_AUTOUPDATE") == "" {
		go autoUpdate(context.Background(), updateConfig{dir: defaultUpdateDir(), current: version})
	}

	roots := splitRoots(*rootsArg)
	st := newStore(roots)
	h := newHub()
	log.Printf("gitknown: scanning %v -> %d repos", roots, len(st.repos()))

	// Watcher: one recursive FSEvents watch per root pushes events the instant
	// the filesystem changes; we map each path to its repo and broadcast.
	go watch(context.Background(), st, h, watchConfig{debounce: *debounce, heartbeat: *heartbeat, poll: *poll})

	a := &api{store: st, hub: h}
	mux := http.NewServeMux()
	mux.Handle("/api/", a.routes())
	mux.Handle("/", staticHandler(*webDir))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // slowloris guard; no WriteTimeout (SSE is long-lived)
	}
	log.Printf("gitknown: http://%s", *addr)
	log.Fatal(srv.ListenAndServe())
}

// watchConfig tunes the watcher's timers. heartbeat and poll are optional
// (0 disables each).
type watchConfig struct {
	debounce  time.Duration // coalesce window for bursts of FS events
	heartbeat time.Duration // liveness log interval; proves the stream is alive
	poll      time.Duration // fallback full re-fingerprint when FSEvents goes silent
}

// watch turns filesystem change events into repo-changed broadcasts. It places
// one recursive FSEvents watch per root (kernel-coalesced, no per-directory
// descriptors), maps each event path back to its repo, and debounces bursts:
// within a window each touched repo is re-fingerprinted once and broadcast only
// if its change set actually differs. .git churn is watched too, so commits and
// pushes update the ahead marker; never-tracked high-churn dirs are skipped. A
// .git entry appearing outside every known repo triggers a rescan, so a repo
// cloned/created (or a worktree added) while running is discovered live.
//
// Two backstops guard the failure mode where FSEvents silently stops delivering
// (sleep, a wedged stream) with the process still alive, so KeepAlive never
// relaunches it and nothing is logged: a heartbeat logs liveness + recent event
// counts so a stall is diagnosable, and a fallback poll re-fingerprints every
// repo on its own timer so changes still surface (at poll latency) regardless.
func watch(ctx context.Context, st *store, h *hub, cfg watchConfig) {
	c := make(chan notify.EventInfo, 1024)
	for _, root := range st.roots {
		if err := notify.Watch(filepath.Join(realPath(root), "..."), c, notify.All); err != nil {
			log.Printf("gitknown: watch %s: %v", root, err)
		}
	}
	defer notify.Stop(c)

	// Repo paths, longest first so the most specific working tree wins for
	// nested repos/worktrees. Paths are canonicalized: FSEvents always reports
	// the realpath, so a symlinked root component (e.g. /tmp -> /private/tmp)
	// would otherwise never prefix-match. rebuild re-derives this from the store
	// so a rescan (new repo discovered) takes effect immediately.
	type repoPath struct{ path, id string }
	var paths []repoPath
	rebuild := func() {
		repos := st.repos()
		paths = make([]repoPath, len(repos))
		for i, r := range repos {
			paths[i] = repoPath{realPath(r.Path), r.ID}
		}
		sort.Slice(paths, func(i, j int) bool { return len(paths[i].path) > len(paths[j].path) })
	}
	rebuild()
	repoFor := func(p string) string {
		for _, e := range paths {
			if p == e.path || strings.HasPrefix(p, e.path+string(os.PathSeparator)) {
				return e.id
			}
		}
		return ""
	}

	// events/broadcasts tally activity since the last heartbeat; the heartbeat
	// logs and resets them. Funnel every broadcast through this counter so the
	// heartbeat can report "saw N events, sent M broadcasts": file edits with
	// events stuck at 0 mean the kernel stopped delivering.
	var events, broadcasts int
	broadcast := func(id string) {
		broadcasts++
		h.broadcast(id)
	}

	// last holds each repo's primed signature so the first real change is
	// detected rather than swallowed as the baseline. primeNew primes any repo
	// not yet seen and, when announce is set, broadcasts it so the UI picks up a
	// newly discovered repo (the baseline prime stays silent).
	last := map[string]string{}
	primeNew := func(announce bool) {
		for _, r := range st.repos() {
			if _, ok := last[r.ID]; ok {
				continue
			}
			last[r.ID] = statusSignature(r.Path, r.Base)
			if announce {
				broadcast(r.ID)
			}
		}
	}
	primeNew(false)

	// syncRepo re-fingerprints one repo and broadcasts when its change set
	// differs; it reports the repo gone when its working tree has vanished
	// (worktree removed / repo deleted) so the caller can drop it before it
	// lingers as a phantom. Shared by the debounced event path and the poll.
	syncRepo := func(id string) (gone bool) {
		r, ok := st.get(id)
		if !ok {
			return false
		}
		if _, err := os.Stat(r.Path); err != nil {
			return true
		}
		sig := statusSignature(r.Path, r.Base)
		if last[id] != sig {
			st.refreshOne(id)
			broadcast(id)
		}
		last[id] = sig
		return false
	}
	dropGone := func(gone []string) {
		if len(gone) == 0 {
			return
		}
		st.refresh()
		rebuild()
		for _, id := range gone {
			delete(last, id)
			broadcast(id)
		}
	}

	gitSeg := pathSeg(".git")                      // "/.git/"  (a child path)
	gitSuffix := string(os.PathSeparator) + ".git" // "/.git"   (the entry itself)

	pending := map[string]bool{}
	rescan := false
	tick := time.NewTicker(cfg.debounce)
	defer tick.Stop()
	heartbeat := newOptionalTicker(cfg.heartbeat)
	defer heartbeat.Stop()
	poll := newOptionalTicker(cfg.poll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c:
			events++
			p := ev.Path()
			if skipWatch(p) {
				continue
			}
			if id := repoFor(p); id != "" {
				pending[id] = true
				continue
			}
			// A path under a root but outside every known repo: a new repo (or
			// worktree) may have appeared. Its creation always touches a .git
			// entry, so gate on that to avoid a full rescan on ordinary churn.
			if strings.Contains(p, gitSeg) || strings.HasSuffix(p, gitSuffix) {
				rescan = true
			}
		case <-tick.C:
			if rescan {
				rescan = false
				st.refresh()
				rebuild()
				primeNew(true)
			}
			var gone []string
			for id := range pending {
				delete(pending, id)
				if syncRepo(id) {
					gone = append(gone, id)
				}
			}
			dropGone(gone)
		case <-poll.C:
			// Backstop for a silently-dead FSEvents stream: re-fingerprint every
			// known repo so edits still surface even when no event ever arrived.
			// In the healthy case the signatures match and nothing broadcasts.
			//
			// Re-run discovery first so a repo that dropped out of the store
			// (its base was momentarily unresolvable during a git op, so a
			// rescan rebuilt the list without it) is picked back up: an idle
			// tree never writes .git again to retrigger the event-path rescan,
			// so the poll is its only way home. primeNew(true) announces any
			// repo this brings back.
			st.refresh()
			rebuild()
			primeNew(true)
			var gone []string
			for _, r := range st.repos() {
				if syncRepo(r.ID) {
					gone = append(gone, r.ID)
				}
			}
			dropGone(gone)
		case <-heartbeat.C:
			log.Printf("gitknown: watcher alive: %d events, %d broadcasts since last beat (repos=%d)", events, broadcasts, len(st.repos()))
			events, broadcasts = 0, 0
		}
	}
}

// optionalTicker is a time.Ticker that may be disabled: when d <= 0 its C
// channel stays nil, so the select arm reading it simply never fires.
type optionalTicker struct {
	t *time.Ticker
	C <-chan time.Time
}

func newOptionalTicker(d time.Duration) optionalTicker {
	if d <= 0 {
		return optionalTicker{}
	}
	t := time.NewTicker(d)
	return optionalTicker{t: t, C: t.C}
}

func (o optionalTicker) Stop() {
	if o.t != nil {
		o.t.Stop()
	}
}

// watchSkip are path segments whose churn would storm the watcher without ever
// affecting a repo's tracked/untracked change set (they are always gitignored).
// .git is deliberately not here: commits and pushes touch it and must update
// the ahead marker. Build dirs that are sometimes committed (dist, vendor,
// target) are left in so their tracked changes still surface.
var watchSkip = []string{
	pathSeg("node_modules"),
	pathSeg(".venv"),
	pathSeg(".next"),
	pathSeg(".direnv"),
}

func pathSeg(name string) string {
	return string(os.PathSeparator) + name + string(os.PathSeparator)
}

// realPath resolves symlinks (and makes absolute) so paths line up with the
// canonical paths FSEvents emits. Falls back to the abs path if resolution
// fails (e.g. the dir does not exist yet).
func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func skipWatch(p string) bool {
	for _, s := range watchSkip {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

func splitRoots(arg string) []string {
	if arg == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return []string{"."}
		}
		return []string{cwd}
	}
	var roots []string
	for _, p := range strings.Split(arg, ",") {
		if p = strings.TrimSpace(p); p != "" {
			roots = append(roots, p)
		}
	}
	return roots
}

// staticHandler serves the embedded SPA build, or a disk dir in dev.
func staticHandler(webDir string) http.Handler {
	if webDir != "" {
		return spaFallback(http.Dir(webDir))
	}
	sub, err := fs.Sub(embedded, "web/dist")
	if err != nil {
		log.Fatal(err)
	}
	return spaFallback(http.FS(sub))
}

// spaFallback serves files, falling back to index.html for client routes.
func spaFallback(fsys http.FileSystem) http.Handler {
	fileServer := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, err := fsys.Open(strings.TrimPrefix(r.URL.Path, "/")); err == nil {
			if cerr := f.Close(); cerr != nil { // opened only to test existence
				log.Printf("gitknown: close probed static file: %v", cerr)
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
