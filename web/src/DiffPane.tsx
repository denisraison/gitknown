import { createEffect, onCleanup } from "solid-js";
import { File, FileDiff } from "@pierre/diffs";
import { fetchFileDiff, fetchFileView, type FileEntry } from "./api";

const THEME = { dark: "pierre-dark", light: "pierre-light" };

// DiffPane wraps the imperative @pierre/diffs viewers. Solid components run once,
// so we own the instance directly: rebuild it when the target file changes,
// clean it up on unmount. No reconciliation fights the widget.
export function DiffPane(props: {
  repoId?: string | undefined;
  file?: FileEntry | undefined;
  diffStyle: "split" | "unified";
  wrap: boolean;
}) {
  let container!: HTMLDivElement;
  let instance: File | FileDiff | undefined;

  createEffect(() => {
    const repoId = props.repoId;
    const file = props.file;
    // overflow/diffStyle are read here so toggling either rebuilds the viewer
    // (the widget is imperative; we own the instance and re-render it).
    const overflow = props.wrap ? "wrap" : "scroll";
    const diffStyle = props.diffStyle;
    if (!repoId || !file) {
      instance?.cleanUp();
      instance = undefined;
      return;
    }
    // Empty status = an unchanged context file (from "all files" mode): there is
    // no diff, so render it with the plain File viewer. Routing it through
    // FileDiff with identical old/new produces zero hunks and shows nothing.
    if (file.status === "") {
      fetchFileView(repoId, file.path).then((d) => {
        instance?.cleanUp();
        const f = new File({ theme: THEME, overflow });
        f.render({
          file: { name: file.path, contents: d.newContents },
          containerWrapper: container,
        });
        instance = f;
      });
      return;
    }
    fetchFileDiff(repoId, file.path, file.status).then((d) => {
      instance?.cleanUp();
      const fd = new FileDiff({ theme: THEME, diffStyle, overflow });
      fd.render({
        oldFile: { name: file.path, contents: d.oldContents },
        newFile: { name: file.path, contents: d.newContents },
        containerWrapper: container,
      });
      instance = fd;
    });
  });

  onCleanup(() => instance?.cleanUp());

  return <div class="pane diff-pane" ref={container} />;
}
