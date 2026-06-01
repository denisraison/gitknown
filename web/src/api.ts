export interface Repo {
  id: string;
  path: string;
  name: string;
  branch: string;
  base: string;
  baseLabel: string;
  ahead: number;
  changedFiles: number;
  dirty: boolean;
}

export interface FileEntry {
  path: string;
  status: string; // M A D R ?
}

export interface FileDiffData {
  path: string;
  status: string;
  oldContents: string;
  newContents: string;
}

// Maps our single-letter git status to @pierre/trees GitStatus values.
export const TREE_STATUS: Record<string, string> = {
  M: "modified",
  A: "added",
  D: "deleted",
  R: "renamed",
  "?": "untracked",
};

const json = (r: Response) => {
  if (!r.ok) {
    throw new Error(`${r.status} ${r.statusText}`);
  }
  return r.json();
};

export const fetchRepos = (): Promise<Repo[]> => fetch("/api/repos").then(json);

export const fetchFiles = (id: string): Promise<FileEntry[]> =>
  fetch(`/api/repos/${id}/files`).then(json);

export const fetchFileDiff = (id: string, path: string, status: string): Promise<FileDiffData> =>
  fetch(`/api/repos/${id}/file?path=${encodeURIComponent(path)}&status=${status}`).then(json);

// fetchFileView gets an unchanged file's current contents (no diff) for context.
export const fetchFileView = (id: string, path: string): Promise<FileDiffData> =>
  fetch(`/api/repos/${id}/file?path=${encodeURIComponent(path)}&mode=view`).then(json);

export interface RepoTree {
  paths: string[];
  capped: boolean; // repo exceeded the server cap; paths is empty
}

// fetchTree gets every file in the repo (tracked + untracked, gitignore-respected).
export const fetchTree = (id: string): Promise<RepoTree> =>
  fetch(`/api/repos/${id}/tree`).then(json);

// subscribeChanges streams repo ids that changed on disk. onReconnect fires when
// the stream re-opens after a drop (backend restart, sleep, network blip): events
// during the gap are lost, so the caller must resync state. Returns unsubscribe.
export const subscribeChanges = (
  onChanged: (repoId: string) => void,
  onReconnect: () => void,
): (() => void) => {
  const es = new EventSource("/api/events");
  let opened = false;
  es.addEventListener("open", () => {
    if (opened) {
      onReconnect();
    }
    opened = true; // first open is the initial connect; later ones are reconnects
  });
  es.addEventListener("changed", (e) => onChanged((e as MessageEvent).data));
  return () => es.close();
};
