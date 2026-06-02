import { createEffect, onCleanup } from "solid-js";
import { File, FileDiff } from "@pierre/diffs";
import { fetchFileDiff, fetchFileView, type FileEntry } from "./api";

const THEME = { dark: "pierre-dark", light: "pierre-light" };

export interface MountDiffArgs {
  path: string;
  status: string;
  oldContents: string;
  newContents: string;
  diffStyle: "split" | "unified";
  overflow: "wrap" | "scroll";
}

// mountDiff renders one file into a container with the imperative @pierre/diffs
// viewers and returns the instance for cleanup. Empty status = an unchanged context
// file (from "all files" mode): no diff, so use the plain File viewer; routing it
// through FileDiff with identical sides produces zero hunks and shows nothing.
// Shared by DiffPane (single-file) and StackedDiffPane (one per stacked row).
export function mountDiff(container: HTMLElement, args: MountDiffArgs): File | FileDiff {
  if (args.status === "") {
    const f = new File({ theme: THEME, overflow: args.overflow });
    f.render({
      file: { name: args.path, contents: args.newContents },
      containerWrapper: container,
    });
    return f;
  }
  const fd = new FileDiff({ theme: THEME, diffStyle: args.diffStyle, overflow: args.overflow });
  fd.render({
    oldFile: { name: args.path, contents: args.oldContents },
    newFile: { name: args.path, contents: args.newContents },
    containerWrapper: container,
  });
  return fd;
}

// DiffPane wraps the imperative viewers for the single-file view. Solid components
// run once, so we own the instance directly: rebuild it when the target file
// changes, clean it up on unmount. No reconciliation fights the widget.
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
    const fetcher =
      file.status === ""
        ? fetchFileView(repoId, file.path)
        : fetchFileDiff(repoId, file.path, file.status);
    fetcher.then((d) => {
      instance?.cleanUp();
      instance = mountDiff(container, {
        path: file.path,
        status: file.status,
        oldContents: d.oldContents,
        newContents: d.newContents,
        diffStyle,
        overflow,
      });
    });
  });

  onCleanup(() => instance?.cleanUp());

  return <div class="pane diff-pane" ref={container} />;
}
