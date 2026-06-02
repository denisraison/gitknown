package main

import (
	"path/filepath"
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
