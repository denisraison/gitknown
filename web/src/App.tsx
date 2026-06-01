import { createSignal, For, onMount, onCleanup, Show } from "solid-js";
import {
  fetchRepos,
  fetchFiles,
  fetchTree,
  subscribeChanges,
  type Repo,
  type FileEntry,
  type RepoTree,
} from "./api";
import { FileTreePane } from "./FileTreePane";
import { DiffPane } from "./DiffPane";

export function App() {
  const [repos, setRepos] = createSignal<Repo[]>([]);
  const [repoId, setRepoId] = createSignal<string>();
  const [files, setFiles] = createSignal<FileEntry[]>([]);
  const [file, setFile] = createSignal<FileEntry>();
  const [query, setQuery] = createSignal("");
  const [showAll, setShowAll] = createSignal(false);
  const [tree, setTree] = createSignal<RepoTree>();

  const loadRepos = () => fetchRepos().then(setRepos);
  const loadFiles = (id: string) => fetchFiles(id).then(setFiles);

  // updateURL mirrors the current view into the query string (via replaceState,
  // so it doesn't spam history) so a refresh restores exactly what's open.
  // Every click that changes state calls this.
  const updateURL = () => {
    const p = new URLSearchParams();
    const r = repoId();
    if (r) {
      p.set("repo", r);
    }
    const f = file();
    if (f) {
      p.set("file", f.path);
    }
    if (showAll()) {
      p.set("all", "1");
    }
    const q = query().trim();
    if (q) {
      p.set("q", q);
    }
    const qs = p.toString();
    history.replaceState(null, "", qs ? `?${qs}` : location.pathname);
  };

  const selectRepo = (id: string) => {
    setRepoId(id);
    setFile(undefined);
    setFiles([]);
    setTree(undefined);
    loadFiles(id);
    if (showAll()) {
      fetchTree(id).then(setTree);
    }
    updateURL();
  };

  const selectFile = (f: FileEntry) => {
    setFile(f);
    updateURL();
  };

  // Toggle "all files": lazy-fetch the full repo listing on first use so the
  // common changes-only path never pays for it.
  const toggleAll = (next: boolean) => {
    setShowAll(next);
    const id = repoId();
    if (next && id && !tree()) {
      fetchTree(id).then(setTree);
    }
    updateURL();
  };

  const setFilter = (q: string) => {
    setQuery(q);
    updateURL();
  };

  // Restore the full view from the URL on load: repo, open file, all-files mode,
  // and the repo filter. ?repo=first&file=first pick the first dirty repo/file.
  const restoreFromURL = (list: Repo[]) => {
    const p = new URLSearchParams(location.search);
    const wantQuery = p.get("q");
    if (wantQuery) {
      setQuery(wantQuery);
    }
    const wantAll = p.get("all") === "1";
    if (wantAll) {
      setShowAll(true);
    }

    const wantRepo = p.get("repo");
    const wantFile = p.get("file");
    const target =
      wantRepo === "first" ? list.find((r) => r.dirty) : list.find((r) => r.id === wantRepo);
    if (!target) {
      return;
    }
    setRepoId(target.id);
    if (wantAll) {
      fetchTree(target.id).then(setTree);
    }
    fetchFiles(target.id).then((fs) => {
      setFiles(fs);
      if (!wantFile) {
        return;
      }
      if (wantFile === "first") {
        if (fs[0]) {
          setFile(fs[0]);
        }
        return;
      }
      // Not in the change set: an unchanged context file (all-files mode), which
      // DiffPane renders via the view endpoint when status is "".
      setFile(fs.find((x) => x.path === wantFile) ?? { path: wantFile, status: "" });
    });
  };

  onMount(() => {
    fetchRepos().then((list) => {
      setRepos(list);
      restoreFromURL(list);
    });
    // Live updates: when a repo changes on disk, refresh counts, and if it's
    // the open one, refresh its file list (the diff pane re-fetches itself).
    const unsub = subscribeChanges((changedId) => {
      loadRepos();
      if (changedId === repoId()) {
        loadFiles(changedId);
      }
    });
    onCleanup(unsub);
  });

  const dirty = () => repos().filter((r) => r.dirty);

  const visible = () => {
    const q = query().trim().toLowerCase();
    const list = dirty();
    if (!q) {
      return list;
    }
    return list.filter(
      (r) => r.name.toLowerCase().includes(q) || r.branch.toLowerCase().includes(q),
    );
  };

  return (
    <div class="app">
      <aside class="repos">
        <div class="repos-top">
          <header class="repos-header">
            changes
            <span class="count">{visible().length}</span>
          </header>
          <input
            class="repos-filter"
            type="search"
            placeholder="filter by name or branch"
            value={query()}
            onInput={(e) => setFilter(e.currentTarget.value)}
          />
        </div>
        <Show
          when={visible().length}
          fallback={<div class="repos-empty">{query().trim() ? "no matches" : "no changes"}</div>}
        >
          <For each={visible()}>
            {(r) => (
              <button
                class="repo"
                classList={{ active: r.id === repoId() }}
                onClick={() => selectRepo(r.id)}
              >
                <span class="repo-name">{r.name}</span>
                <span class="repo-meta">
                  <span class="badge">{r.changedFiles}</span>
                  <Show when={r.ahead > 0}>
                    <span class="ahead">↑{r.ahead}</span>
                  </Show>
                </span>
                <span class="repo-branch">
                  {r.branch} · {r.baseLabel}
                </span>
              </button>
            )}
          </For>
        </Show>
      </aside>

      <FileTreePane
        files={files()}
        allPaths={tree()?.paths}
        capped={tree()?.capped ?? false}
        showAll={showAll()}
        onToggle={toggleAll}
        onSelect={selectFile}
      />

      <main class="diff-wrap">
        <Show when={file()} fallback={<div class="empty">select a file</div>}>
          <div class="diff-header">{file()!.path}</div>
          <DiffPane repoId={repoId()} file={file()} />
        </Show>
      </main>
    </div>
  );
}
