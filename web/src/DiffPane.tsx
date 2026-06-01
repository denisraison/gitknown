import { createEffect, onCleanup } from "solid-js";
import { FileDiff } from "@pierre/diffs";
import { fetchFileDiff, fetchFileView, type FileEntry } from "./api";

// DiffPane wraps the imperative @pierre/diffs core. Solid components run once,
// so we own the FileDiff instance directly: rebuild it when the target file
// changes, clean it up on unmount. No reconciliation fights the widget.
export function DiffPane(props: { repoId?: string | undefined; file?: FileEntry | undefined }) {
  let container!: HTMLDivElement;
  let instance: FileDiff | undefined;

  createEffect(() => {
    const repoId = props.repoId;
    const file = props.file;
    if (!repoId || !file) {
      instance?.cleanUp();
      instance = undefined;
      return;
    }
    // Empty status = an unchanged context file (from "all files" mode): no
    // diff exists, fetch its plain contents and render it as a no-op diff.
    const load =
      file.status === ""
        ? fetchFileView(repoId, file.path)
        : fetchFileDiff(repoId, file.path, file.status);
    load.then((d) => {
      instance?.cleanUp();
      instance = new FileDiff({
        theme: { dark: "pierre-dark", light: "pierre-light" },
        diffStyle: "split",
      });
      instance.render({
        oldFile: { name: file.path, contents: d.oldContents },
        newFile: { name: file.path, contents: d.newContents },
        containerWrapper: container,
      });
    });
  });

  onCleanup(() => instance?.cleanUp());

  return <div class="pane diff-pane" ref={container} />;
}
