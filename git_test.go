package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverReposFindsLinkedWorktree covers a scan root holding several
// working trees side by side, one of which is a linked git worktree. A linked
// worktree's .git is a *file* (pointing into the main repo's .git/worktrees),
// not a dir, so this guards the claim in discoverRepos that statting any .git
// entry captures worktrees too. The layout mirrors a real user's tree: a few
// plain clones next to a worktree dir.
func TestDiscoverReposFindsLinkedWorktree(t *testing.T) {
	root := t.TempDir()

	dir1 := filepath.Join(root, "dir1")
	dir2 := filepath.Join(root, "dir2")
	dir4 := filepath.Join(root, "dir4")
	mustGitRepo(t, dir1)
	mustGitRepo(t, dir2)
	mustGitRepo(t, dir4)

	// _worktree is a linked worktree of dir1, sitting as a sibling of the plain
	// clones. git puts the real gitdir under dir1/.git/worktrees and leaves a
	// .git file at the worktree root.
	worktree := filepath.Join(root, "_worktree")
	runGit(t, dir1, "branch", "feature")
	runGit(t, dir1, "worktree", "add", "-q", worktree, "feature")

	repos := discoverRepos([]string{root})

	got := make(map[string]bool, len(repos))
	for _, r := range repos {
		got[r.Path] = true
	}
	for _, want := range []string{dir1, dir2, dir4, worktree} {
		if !got[want] {
			t.Errorf("discoverRepos missing %q", want)
		}
	}
	if len(repos) != 4 {
		t.Fatalf("discoverRepos: got %d repos, want 4: %+v", len(repos), repos)
	}
}

// TestResolveBaseUntrackedBranchHidesCommittedWork is the regression guard for
// the bug a user hit: a feature branch with no resolvable upstream (its
// remote-tracking ref isn't present locally) used to fall back to origin/main,
// so the branch's whole committed history showed as "changed" forever even with
// a clean working tree. With no upstream the base is now HEAD, so only
// uncommitted changes show; committed work stays hidden until it's pushable.
func TestResolveBaseUntrackedBranchHidesCommittedWork(t *testing.T) {
	repo := t.TempDir()
	mustGitRepo(t, repo)
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(repo, "feature.txt"), "work\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "feature work")

	r, ok := loadRepo(repo, repo)
	if !ok {
		t.Fatal("loadRepo failed")
	}
	if r.BaseLabel != "working tree" {
		t.Fatalf("base = %q, want HEAD fallback for an untracked branch", r.BaseLabel)
	}
	if r.Dirty || r.ChangedFiles != 0 || r.Ahead != 0 {
		t.Fatalf("clean untracked branch shows committed work: dirty=%v changed=%d ahead=%d", r.Dirty, r.ChangedFiles, r.Ahead)
	}

	// An uncommitted edit must still surface.
	write(t, filepath.Join(repo, "feature.txt"), "more\n")
	if r, _ = loadRepo(repo, repo); !r.Dirty || r.ChangedFiles != 1 {
		t.Fatalf("uncommitted edit not shown: dirty=%v changed=%d", r.Dirty, r.ChangedFiles)
	}
}

// TestResolveBaseTrackedBranchShowsUnpushed: a branch tracking a remote diffs
// against that upstream, so an unpushed commit (and its files) shows as the
// change set, and Ahead counts it.
func TestResolveBaseTrackedBranchShowsUnpushed(t *testing.T) {
	remote := t.TempDir()
	runGit(t, remote, "init", "-q", "--bare")
	repo := t.TempDir()
	mustGitRepo(t, repo)
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "-u", "origin", "HEAD")

	write(t, filepath.Join(repo, "next.txt"), "x\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "unpushed")

	r, ok := loadRepo(repo, repo)
	if !ok {
		t.Fatal("loadRepo failed")
	}
	if !strings.HasPrefix(r.BaseLabel, "origin/") {
		t.Fatalf("base = %q, want the branch upstream", r.BaseLabel)
	}
	if r.Ahead != 1 || r.ChangedFiles != 1 || !r.Dirty {
		t.Fatalf("unpushed commit not shown: ahead=%d changed=%d dirty=%v", r.Ahead, r.ChangedFiles, r.Dirty)
	}
}

// TestDiscoverReposDescendsIntoUnderscoreDir guards the case a user actually
// hit: a plain "_worktree" container folder holding several working trees
// (here linked worktrees of a main repo). The walker has no underscore/hidden
// rule, so it descends into "_worktree" and surfaces every tree inside. The
// folders not showing up in the UI is a separate concern (the sidebar only
// renders dirty repos); discovery itself must find them.
func TestDiscoverReposDescendsIntoUnderscoreDir(t *testing.T) {
	root := t.TempDir()

	main := filepath.Join(root, "main")
	mustGitRepo(t, main)
	runGit(t, main, "branch", "feature-a")
	runGit(t, main, "branch", "feature-b")

	// _worktree is just a directory, not a working tree itself; the repos live
	// one level below it.
	wtA := filepath.Join(root, "_worktree", "feature-a")
	wtB := filepath.Join(root, "_worktree", "feature-b")
	runGit(t, main, "worktree", "add", "-q", wtA, "feature-a")
	runGit(t, main, "worktree", "add", "-q", wtB, "feature-b")

	repos := discoverRepos([]string{root})

	got := make(map[string]bool, len(repos))
	for _, r := range repos {
		got[r.Path] = true
	}
	for _, want := range []string{main, wtA, wtB} {
		if !got[want] {
			t.Errorf("discoverRepos missing %q (did not descend into _worktree)", want)
		}
	}
}
