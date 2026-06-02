import { createEffect, onCleanup, Show } from "solid-js";
import { FileTree } from "@pierre/trees";
import { TREE_STATUS, type FileEntry } from "./api";

// sameFiles reports whether two change sets are identical in path + status, so
// the tree only rebuilds when the displayed set actually changed.
function sameFiles(a: FileEntry[], b: FileEntry[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  return a.every((x, i) => x.path === b[i]?.path && x.status === b[i]?.status);
}

// ancestorDirs collects every directory path that appears in the given file
// paths (each path's ancestors). Used to seed "all files" expansion and to walk
// a folder's descendants when folding it.
function ancestorDirs(paths: string[]): string[] {
  const dirs = new Set<string>();
  for (const p of paths) {
    const parts = p.split("/");
    parts.pop();
    let acc = "";
    for (const part of parts) {
      acc = acc ? `${acc}/${part}` : part;
      dirs.add(acc);
    }
  }
  return [...dirs];
}

// changedDirs is the ancestor set of the changed files, so "all files" mode can
// start collapsed yet expand the folders that contain changes.
const changedDirs = (files: FileEntry[]): string[] => ancestorDirs(files.map((f) => f.path));

// Wraps the imperative @pierre/trees core. "all files" mode shows the whole repo
// with changed files badged; its unchanged files carry status "" so the caller
// fetches a plain view instead of a diff.
export function FileTreePane(props: {
  files: FileEntry[];
  allPaths?: string[] | undefined;
  capped: boolean;
  showAll: boolean;
  onToggle: (showAll: boolean) => void;
  onSelect: (file: FileEntry) => void;
}) {
  let container!: HTMLDivElement;
  let tree: FileTree | undefined;
  let unsub: (() => void) | undefined;
  let last: { files: FileEntry[]; all: string[] | undefined; showAll: boolean } | undefined;

  createEffect(() => {
    const files = props.files;
    const all = props.allPaths;
    const showAll = props.showAll;
    // The effect re-runs on any tab change (selecting a file replaces the tab
    // object), but the tree only needs rebuilding when the displayed set changes
    // — otherwise a mere selection would collapse the user's expansion. allPaths
    // is stable by reference except on toggle/tab-switch, so compare it directly.
    if (last && last.all === all && last.showAll === showAll && sameFiles(last.files, files)) {
      return;
    }
    last = { files, all, showAll };
    const useAll = showAll && !!all && all.length > 0;
    unsub?.();
    unsub = undefined;
    tree?.cleanUp();

    const changed = new Map(files.map((f) => [f.path, f]));
    const paths = useAll ? all : files.map((f) => f.path);
    if (paths.length === 0) {
      tree = undefined;
      return;
    }

    tree = new FileTree({
      paths,
      gitStatus: files.map((f) => ({
        path: f.path,
        status: (TREE_STATUS[f.status] ?? "modified") as never,
      })),
      // Changes-only: show every file up front. All-files: collapse the repo
      // but expand the folders holding changes so the change set stays visible.
      initialExpansion: useAll ? "closed" : "open",
      ...(useAll ? { initialExpandedPaths: changedDirs(files) } : {}),
      search: true,
      onSelectionChange: (selected) => {
        const first = selected[0];
        if (first == null) {
          return;
        }
        const p = String(first);
        if (tree?.getItem(p)?.isDirectory()) {
          return; // folders aren't selectable targets
        }
        props.onSelect(changed.get(p) ?? { path: p, status: "" });
      },
    });
    tree.render({ fileTreeContainer: container });

    // Folding a folder should fold everything inside it, so re-expanding it shows
    // its children collapsed, not the deep state you left behind. The widget keeps
    // a descendant's expansion across a parent collapse and exposes no expand/
    // collapse event, so we diff expansion on the generic change listener: any
    // directory we last saw open that's now closed gets its descendant directories
    // collapsed too. Search is skipped — the widget collapses/restores expansion
    // internally then, and those aren't user folds.
    const view = tree;
    const dirs = ancestorDirs(paths);
    const openDirs = new Set<string>();
    let syncing = false;
    let prevSearching = false;
    const currentlyOpen = () => {
      const open = new Set<string>();
      for (const d of dirs) {
        const h = view.getItem(d);
        if (h && "isExpanded" in h && h.isExpanded()) {
          open.add(d);
        }
      }
      return open;
    };
    unsub = view.subscribe(() => {
      if (syncing) {
        return;
      }
      const open = currentlyOpen();
      const searching = view.isSearchOpen() || view.getSearchValue().length > 0;
      // Skip the clear-search emit too (prevSearching): the widget restores
      // expansion then, and that restore isn't a user fold.
      if (!searching && !prevSearching) {
        const folded = [...openDirs].filter((d) => !open.has(d));
        if (folded.length > 0) {
          syncing = true;
          for (const parent of folded) {
            const prefix = `${parent}/`;
            for (const d of dirs) {
              if (!d.startsWith(prefix)) {
                continue;
              }
              const h = view.getItem(d);
              if (h && "isExpanded" in h && h.isExpanded()) {
                h.collapse();
                open.delete(d);
              }
            }
          }
          syncing = false;
        }
      }
      prevSearching = searching;
      openDirs.clear();
      for (const d of open) {
        openDirs.add(d);
      }
    });
  });

  onCleanup(() => {
    unsub?.();
    tree?.cleanUp();
  });

  return (
    <div class="pane tree-pane">
      <div class="tree-head">
        <button
          class="tree-toggle"
          classList={{ active: props.showAll }}
          onClick={() => props.onToggle(!props.showAll)}
        >
          {props.showAll ? "all files" : "changes"}
        </button>
        <Show when={props.showAll && props.capped}>
          <span class="tree-note">too many files, showing changes</span>
        </Show>
      </div>
      <div class="tree-body" ref={container} />
    </div>
  );
}
