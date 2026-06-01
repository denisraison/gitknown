import { createEffect, onCleanup, Show } from "solid-js";
import { FileTree } from "@pierre/trees";
import { TREE_STATUS, type FileEntry } from "./api";

// changedDirs collects every ancestor directory of the changed files, so "all
// files" mode can start collapsed yet expand the folders that contain changes.
function changedDirs(files: FileEntry[]): string[] {
  const dirs = new Set<string>();
  for (const f of files) {
    const parts = f.path.split("/");
    parts.pop(); // drop the filename
    let acc = "";
    for (const part of parts) {
      acc = acc ? `${acc}/${part}` : part;
      dirs.add(acc);
    }
  }
  return [...dirs];
}

// FileTreePane wraps the imperative @pierre/trees core. Rebuilds the tree when
// the file set (or mode) changes; reports selection back up via onSelect.
// "changes" mode shows only the change set; "all files" mode shows the whole
// repo with the changed files still badged. Unchanged files report status ""
// so the caller knows to fetch a plain view instead of a diff.
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

  createEffect(() => {
    const files = props.files;
    const all = props.allPaths;
    const useAll = props.showAll && !!all && all.length > 0;
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
  });

  onCleanup(() => tree?.cleanUp());

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
