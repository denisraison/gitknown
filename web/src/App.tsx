import { createSignal, createMemo, For, onMount, onCleanup, Show } from "solid-js";
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

// One open repo with its own view state; id is the repo id and the tab key.
interface Tab {
  id: string;
  files: FileEntry[];
  file?: FileEntry;
  showAll: boolean;
  tree?: RepoTree;
}

export function App() {
  const [repos, setRepos] = createSignal<Repo[]>([]);
  const [tabs, setTabs] = createSignal<Tab[]>([]);
  const [activeId, setActiveId] = createSignal<string>();
  const [query, setQuery] = createSignal("");

  const active = createMemo(() => tabs().find((t) => t.id === activeId()));
  const repoById = (id: string) => repos().find((r) => r.id === id);

  // Tree-pane width is a draggable preference: persist it in localStorage (not
  // the URL, which carries view state, not layout).
  const storedWidth = Number(localStorage.getItem("gk.treeWidth"));
  const [treeWidth, setTreeWidth] = createSignal(storedWidth >= 180 ? storedWidth : 280);

  // Window-level listeners (not pointer capture on the bar) so a fast drag never
  // outruns the divider.
  const startResize = (e: PointerEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = treeWidth();
    document.body.style.userSelect = "none";
    document.body.style.cursor = "col-resize";
    const onMove = (ev: PointerEvent) => {
      const max = Math.max(180, window.innerWidth - 360); // always leave room for the diff
      setTreeWidth(Math.min(max, Math.max(180, startW + ev.clientX - startX)));
    };
    const onUp = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      document.body.style.userSelect = "";
      document.body.style.cursor = "";
      localStorage.setItem("gk.treeWidth", String(treeWidth()));
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
  };

  const loadRepos = () => fetchRepos().then(setRepos);

  // patchTab replaces a single tab immutably (others keep their reference, so
  // only the changed tab re-renders). A no-op if the tab was closed meanwhile.
  const patchTab = (id: string, patch: Partial<Tab>) =>
    setTabs((ts) => ts.map((t) => (t.id === id ? { ...t, ...patch } : t)));

  const loadFiles = (id: string) => fetchFiles(id).then((fs) => patchTab(id, { files: fs }));

  // replaceState (not pushState) so live updates don't spam history; a refresh
  // then restores exactly what's open.
  const updateURL = () => {
    const p = new URLSearchParams();
    const open = tabs();
    if (open.length) {
      p.set("tabs", open.map((t) => t.id).join(","));
    }
    const a = active();
    if (a) {
      p.set("repo", a.id);
      if (a.file) {
        p.set("file", a.file.path);
      }
      if (a.showAll) {
        p.set("all", "1");
      }
    }
    const q = query().trim();
    if (q) {
      p.set("q", q);
    }
    const qs = p.toString();
    history.replaceState(null, "", qs ? `?${qs}` : location.pathname);
  };

  const openRepo = (id: string) => {
    if (!tabs().some((t) => t.id === id)) {
      setTabs((ts) => [...ts, { id, files: [], showAll: false }]);
      loadFiles(id);
    }
    setActiveId(id);
    updateURL();
  };

  const closeTab = (id: string) => {
    const cur = tabs();
    const idx = cur.findIndex((t) => t.id === id);
    const next = cur.filter((t) => t.id !== id);
    setTabs(next);
    if (activeId() === id) {
      // Focus the left neighbor, else the one that shifted into its slot.
      setActiveId((next[idx - 1] ?? next[idx])?.id);
    }
    updateURL();
  };

  const selectFile = (f: FileEntry) => {
    const id = activeId();
    if (!id) {
      return;
    }
    patchTab(id, { file: f });
    updateURL();
  };

  // Lazy-fetch the full repo listing on first toggle so the common changes-only
  // path never pays for it.
  const toggleAll = (next: boolean) => {
    const id = activeId();
    if (!id) {
      return;
    }
    patchTab(id, { showAll: next });
    if (next && !active()?.tree) {
      fetchTree(id).then((tr) => patchTab(id, { tree: tr }));
    }
    updateURL();
  };

  const setFilter = (q: string) => {
    setQuery(q);
    updateURL();
  };

  // ?tabs is the open set; fall back to a lone ?repo for older deep links. The
  // "first" sentinel (repo/file) resolves to the first dirty repo/changed file.
  const restoreFromURL = (list: Repo[]) => {
    const p = new URLSearchParams(location.search);
    const wantQuery = p.get("q");
    if (wantQuery) {
      setQuery(wantQuery);
    }
    const wantAll = p.get("all") === "1";
    const wantRepo = p.get("repo");
    const wantFile = p.get("file");
    const tabsParam = p.get("tabs");

    const resolve = (id: string) => (id === "first" ? list.find((r) => r.dirty)?.id : id);
    const raw = tabsParam ? tabsParam.split(",") : wantRepo ? [wantRepo] : [];
    const openIds = [
      ...new Set(
        raw.map(resolve).filter((id): id is string => !!id && list.some((r) => r.id === id)),
      ),
    ];
    if (!openIds.length) {
      return;
    }

    setTabs(openIds.map((id) => ({ id, files: [], showAll: false })));
    const wantActive = wantRepo ? resolve(wantRepo) : undefined;
    const activeTarget = wantActive && openIds.includes(wantActive) ? wantActive : openIds[0];
    setActiveId(activeTarget);

    // Load files for every open tab; restore all-mode + open file only on the
    // active one (the others restore to their default changes-only view).
    openIds.forEach((id) => {
      const isActive = id === activeTarget;
      if (isActive && wantAll) {
        patchTab(id, { showAll: true });
        fetchTree(id).then((tr) => patchTab(id, { tree: tr }));
      }
      fetchFiles(id).then((fs) => {
        patchTab(id, { files: fs });
        if (!isActive || !wantFile) {
          return;
        }
        // "first" picks the first changed file; otherwise match the path, falling
        // back to an unchanged context file (status "") for all-files deep links.
        const f =
          wantFile === "first"
            ? fs[0]
            : (fs.find((x) => x.path === wantFile) ?? { path: wantFile, status: "" });
        if (!f) {
          return;
        }
        patchTab(id, { file: f });
      });
    });
  };

  onMount(() => {
    fetchRepos().then((list) => {
      setRepos(list);
      restoreFromURL(list);
    });
    // On a disk change, refresh counts and prune tabs whose repo vanished (e.g. a
    // removed worktree); the open tab's diff pane re-fetches itself.
    const unsub = subscribeChanges(
      (changedId) => {
        loadRepos().then(() => {
          const live = new Set(repos().map((r) => r.id));
          if (tabs().some((t) => !live.has(t.id))) {
            const kept = tabs().filter((t) => live.has(t.id));
            setTabs(kept);
            if (activeId() && !live.has(activeId()!)) {
              setActiveId(kept[0]?.id);
            }
            updateURL();
          }
          if (live.has(changedId) && tabs().some((t) => t.id === changedId)) {
            loadFiles(changedId);
          }
        });
      },
      // Reconnect after a drop: changes during the gap were missed, so refresh
      // the repo list and every open tab's files from scratch.
      () => {
        loadRepos();
        tabs().forEach((t) => loadFiles(t.id));
      },
    );
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
                classList={{ active: r.id === activeId(), open: tabs().some((t) => t.id === r.id) }}
                onClick={() => openRepo(r.id)}
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

      <div class="workspace">
        <Show when={tabs().length}>
          <div class="tabs" role="tablist">
            <For each={tabs()}>
              {(t) => (
                <div
                  class="tab"
                  classList={{ active: t.id === activeId() }}
                  onClick={() => openRepo(t.id)}
                >
                  <span class="tab-name">{repoById(t.id)?.name ?? t.id}</span>
                  <Show when={(repoById(t.id)?.changedFiles ?? 0) > 0}>
                    <span class="tab-badge">{repoById(t.id)!.changedFiles}</span>
                  </Show>
                  <button
                    class="tab-close"
                    title="close tab"
                    onClick={(e) => {
                      e.stopPropagation();
                      closeTab(t.id);
                    }}
                  >
                    ×
                  </button>
                </div>
              )}
            </For>
          </div>
        </Show>

        <div class="panes" style={{ "grid-template-columns": `${treeWidth()}px 5px 1fr` }}>
          <FileTreePane
            files={active()?.files ?? []}
            allPaths={active()?.tree?.paths}
            capped={active()?.tree?.capped ?? false}
            showAll={active()?.showAll ?? false}
            onToggle={toggleAll}
            onSelect={selectFile}
          />

          <div class="resizer" onPointerDown={startResize} />

          <main class="diff-wrap">
            <Show when={active()?.file} fallback={<div class="empty">select a file</div>}>
              <div class="diff-header">{active()!.file!.path}</div>
              <DiffPane repoId={activeId()} file={active()!.file} />
            </Show>
          </main>
        </div>
      </div>
    </div>
  );
}
