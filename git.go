package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gitTimeout bounds every git invocation so a stuck command (lock wait, huge
// repo) can never wedge a request or the watcher indefinitely.
const gitTimeout = 30 * time.Second

// Repo is a single git working tree (a clone or a linked worktree).
type Repo struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Name         string `json:"name"`   // display name relative to a scan root
	Branch       string `json:"branch"` // current branch or detached sha
	Base         string `json:"base"`   // ref we diff against (upstream / merge-base / HEAD)
	BaseLabel    string `json:"baseLabel"`
	Ahead        int    `json:"ahead"`        // unpushed commits
	ChangedFiles int    `json:"changedFiles"` // files differing from base + untracked
	Dirty        bool   `json:"dirty"`
}

// FileEntry is one changed path in a repo, vs the repo's base ref.
type FileEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"` // M, A, D, R, ?  (single letter)
}

// FileDiff is the old/new content pair the diff renderer consumes directly.
type FileDiff struct {
	Path        string `json:"path"`
	Status      string `json:"status"`
	OldContents string `json:"oldContents"`
	NewContents string `json:"newContents"`
}

// RepoTree is the full file listing for "all files" mode: every path git
// tracks or would not ignore, so the UI can show context beyond the change set.
type RepoTree struct {
	Paths  []string `json:"paths"`
	Capped bool     `json:"capped"` // true when the repo exceeds treeCap; Paths is empty
}

// dirsToSkip are never descended into during discovery.
var dirsToSkip = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	".next":        true,
	"target":       true,
	".venv":        true,
}

// discoverRepos walks the roots and returns every git working tree found.
// A linked worktree carries a .git *file* (not dir), so finding any .git
// entry and treating its parent as a working tree captures worktrees too.
func discoverRepos(roots []string) []Repo {
	seen := map[string]bool{}
	var repos []Repo
	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // unreadable entry: skip it, keep walking
			}
			if !d.IsDir() {
				return nil
			}
			if dirsToSkip[d.Name()] && path != absRoot {
				return filepath.SkipDir
			}
			gitPath := filepath.Join(path, ".git")
			if _, err := os.Stat(gitPath); err != nil {
				return nil //nolint:nilerr // no .git here: not a working tree
			}
			// Found a working tree. Record it and stop descending: nested
			// worktrees live in sibling dirs, not inside the tree itself.
			if !seen[path] {
				seen[path] = true
				if r, ok := loadRepo(path, absRoot); ok {
					repos = append(repos, r)
				}
			}
			return filepath.SkipDir
		})
		if walkErr != nil {
			continue // root itself unreadable; skip it
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos
}

// loadRepo fills the summary for a single working tree.
func loadRepo(path, root string) (Repo, bool) {
	name, err := filepath.Rel(filepath.Dir(root), path)
	if err != nil {
		name = filepath.Base(path)
	}
	r := Repo{
		ID:   idFor(path),
		Path: path,
		Name: name,
	}
	r.Branch = strings.TrimSpace(git(path, "rev-parse", "--abbrev-ref", "HEAD"))
	r.Base, r.BaseLabel = resolveBase(path)
	if r.Base == "" {
		return r, false
	}
	files := changedFiles(path, r.Base)
	r.ChangedFiles = len(files)
	r.Dirty = r.ChangedFiles > 0
	if out := strings.TrimSpace(git(path, "rev-list", "--count", r.Base+"..HEAD")); out != "" {
		r.Ahead = atoi(out)
	}
	return r, true
}

// resolveBase picks the ref to diff against and returns its merge-base with
// HEAD (the fork point). Diffing against the fork point, not the ref's tip,
// means we only ever show what *this* branch added, never changes the base
// advanced past us. Prefers the branch's upstream, else a default branch.
func resolveBase(path string) (ref, label string) {
	var cands []string
	if up := strings.TrimSpace(git(path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")); up != "" {
		cands = append(cands, up)
	}
	cands = append(cands, "origin/HEAD", "origin/main", "origin/master", "main", "master")
	for _, cand := range cands {
		if sha := strings.TrimSpace(git(path, "rev-parse", "--verify", "--quiet", cand)); sha != "" {
			if mb := strings.TrimSpace(git(path, "merge-base", "HEAD", cand)); mb != "" {
				return mb, cand
			}
		}
	}
	if head := strings.TrimSpace(git(path, "rev-parse", "--verify", "--quiet", "HEAD")); head != "" {
		return "HEAD", "working tree"
	}
	return "", ""
}

// changedFiles lists paths differing from base, plus untracked files.
func changedFiles(path, base string) []FileEntry {
	var out []FileEntry
	seen := map[string]bool{}
	// Tracked changes vs base (covers unpushed commits + staged + unstaged).
	nameStatus := git(path, "diff", "--name-status", "-M", base)
	sc := bufio.NewScanner(strings.NewReader(nameStatus))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		status := string(fields[0][0]) // R100 -> R, M -> M
		p := fields[len(fields)-1]     // rename: take the new path
		if !seen[p] {
			seen[p] = true
			out = append(out, FileEntry{Path: p, Status: status})
		}
	}
	// Untracked files.
	others := git(path, "ls-files", "--others", "--exclude-standard")
	sc = bufio.NewScanner(strings.NewReader(others))
	for sc.Scan() {
		p := strings.TrimSpace(sc.Text())
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, FileEntry{Path: p, Status: "?"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// treeCap bounds repoTree. Past this many files we skip the full listing
// rather than ship a giant payload (a repo that doesn't gitignore its build
// output, or a monorepo); the UI falls back to changes-only.
const treeCap = 20000

// repoTree lists every file in the working tree that git tracks or would not
// ignore (tracked ∪ untracked-not-ignored). --exclude-standard is what honors
// .gitignore (plus .git/info/exclude and the global excludes), so this respects
// ignore rules by construction. Returns Capped (and no paths) past treeCap.
func repoTree(path string) RepoTree {
	seen := map[string]bool{}
	for _, out := range [...]string{
		git(path, "ls-files"),
		git(path, "ls-files", "--others", "--exclude-standard"),
	} {
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			p := strings.TrimSpace(sc.Text())
			if p == "" || seen[p] {
				continue
			}
			if len(seen) >= treeCap {
				return RepoTree{Capped: true}
			}
			seen[p] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return RepoTree{Paths: paths}
}

// viewFile returns a path's current working-tree contents as a no-diff pair
// (old == new) so the diff renderer can show an unchanged file as plain
// context. Same repo-root anchoring as fileDiff guards against ../ escapes.
func viewFile(path, rel string) FileDiff {
	rel = strings.TrimPrefix(filepath.Clean("/"+rel), "/")
	d := FileDiff{Path: rel}
	if b, err := os.ReadFile(filepath.Join(path, rel)); err == nil { //nolint:gosec // G304: rel is anchored to the repo root above
		d.OldContents = string(b)
		d.NewContents = d.OldContents
	}
	return d
}

// fileDiff returns the old (base) and new (working tree) contents for a path.
func fileDiff(path, base, rel, status string) FileDiff {
	// Anchor rel at the repo root so a crafted ../ path can't read or `git show`
	// files outside this working tree.
	rel = strings.TrimPrefix(filepath.Clean("/"+rel), "/")
	d := FileDiff{Path: rel, Status: status}
	if status != "?" && status != "A" {
		d.OldContents = git(path, "show", base+":"+rel)
	}
	if status != "D" {
		full := filepath.Join(path, rel)
		if b, err := os.ReadFile(full); err == nil { //nolint:gosec // G304: rel is anchored to the repo root above
			d.NewContents = string(b)
		}
	}
	return d
}

// fnv-1a 64-bit, inlined so hashing has no Write call (and thus no impossible
// error to swallow). A non-crypto fingerprint: a collision only ever means a
// missed refresh, never a security issue.
const (
	fnvOffset uint64 = 14695981039346656037
	fnvPrime  uint64 = 1099511628211
)

func fnv1a(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

// statusSignature is a fingerprint of a repo's change set, used by the watcher
// to detect when anything changed without re-broadcasting on noise. It folds in
// the full `git diff` patch (not just --name-status) so editing the *contents*
// of an already-listed file changes the signature too: porcelain v2 and
// name-status only carry paths/status letters, never the working-tree blob, so
// a second edit of a file already marked M would otherwise look identical and
// the open diff would never refresh.
// --no-optional-locks: never take index.lock for the stat-cache refresh, so the
// watcher fingerprinting a repo can't collide with the user's own git commands.
func statusSignature(path, base string) string {
	h := fnv1a(fnvOffset, git(path, "--no-optional-locks", "status", "--porcelain=v2", "--branch"))
	h = fnv1a(h, git(path, "--no-optional-locks", "diff", base))
	return strconv.FormatUint(h, 16)
}

func git(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: git is a fixed binary; args are ours, not shell-interpreted
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func idFor(path string) string {
	return strconv.FormatUint(fnv1a(fnvOffset, path), 16)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
