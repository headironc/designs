import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  openSync,
  readFileSync,
  realpathSync,
  statSync,
} from "node:fs";
import path from "node:path";

function validatePath(file: string): void {
  if (
    !file ||
    file.includes("\0") ||
    file.includes("\\") ||
    path.posix.isAbsolute(file) ||
    file.split("/").some((part) => !part || [".", "..", ".git"].includes(part))
  )
    throw new Error(`Expected a normalized repository-relative path: ${file}`);
}

/** Include tracked and nonignored new files, excluding working-tree deletions. */
export function listSourcePaths(root: string): string[] {
  const output = execFileSync(
    "git",
    [
      "-c",
      `safe.directory=${realpathSync(root)}`,
      "-c",
      "core.fsmonitor=false",
      "ls-files",
      "-z",
      "--cached",
      "--others",
      "--exclude-standard",
    ],
    {
      cwd: root,
      env: {
        PATH: process.env.PATH,
        LC_ALL: "C",
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
        GIT_OPTIONAL_LOCKS: "0",
      },
      timeout: 30_000,
      maxBuffer: 16 * 1024 * 1024,
    },
  );
  const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
  const files: string[] = [];
  let start = 0;
  for (
    let end = output.indexOf(0);
    end !== -1;
    end = output.indexOf(0, start)
  ) {
    files.push(decoder.decode(output.subarray(start, end)));
    start = end + 1;
  }
  if (start !== output.length) throw new Error("Incomplete Git source listing");
  return [...new Set(files)].filter((file) => {
    validatePath(file);
    try {
      lstatSync(path.join(root, file));
      return true;
    } catch (error) {
      if (
        ["ENOENT", "ENOTDIR"].includes(
          (error as NodeJS.ErrnoException).code ?? "",
        )
      )
        return false;
      throw error;
    }
  });
}

const directoryFlags =
  constants.O_RDONLY |
  constants.O_DIRECTORY |
  constants.O_NOFOLLOW |
  constants.O_NONBLOCK;

/** Bind the root from /, refusing symlinks in every component of its absolute path. */
export function openSourceRoot(root: string): number {
  if (process.platform !== "linux")
    throw new Error("Formatting requires Linux with /proc/self/fd");
  const absolute = path.resolve(root);
  const expected = lstatSync(absolute, { bigint: true });
  if (!expected.isDirectory())
    throw new Error(`Source root must be a real directory: ${absolute}`);
  let directory = openSync("/", directoryFlags);
  try {
    for (const part of absolute.split("/").filter(Boolean)) {
      const child = openSync(
        `/proc/self/fd/${directory}/${part}`,
        directoryFlags,
      );
      closeSync(directory);
      directory = child;
    }
    const opened = fstatSync(directory, { bigint: true });
    const viaProc = statSync(`/proc/self/fd/${directory}`, { bigint: true });
    if (
      opened.ino !== expected.ino ||
      opened.dev !== expected.dev ||
      viaProc.ino !== opened.ino ||
      viaProc.dev !== opened.dev
    )
      throw new Error(`Source root changed while opening: ${absolute}`);
    return directory;
  } catch (error) {
    closeSync(directory);
    throw new Error(
      `Cannot bind source root; Linux /proc/self/fd is required: ${absolute}`,
      { cause: error },
    );
  }
}

/** Each open resolves one name beneath an already held directory, never its ancestors. */
export function openSourceFile(
  root: number,
  file: string,
  write: boolean,
): number {
  validatePath(file);
  const parts = file.split("/");
  const directories: number[] = [];
  let parent = root;
  try {
    for (const part of parts.slice(0, -1)) {
      parent = openSync(`/proc/self/fd/${parent}/${part}`, directoryFlags);
      directories.push(parent);
    }
    const target = `/proc/self/fd/${parent}/${parts.at(-1)!}`;
    const expected = lstatSync(target, { bigint: true });
    if (!expected.isFile())
      throw new Error(`Source must be a regular file: ${file}`);
    const descriptor = openSync(
      target,
      (write ? constants.O_RDWR : constants.O_RDONLY) |
        constants.O_NOFOLLOW |
        constants.O_NONBLOCK,
    );
    try {
      const opened = fstatSync(descriptor, { bigint: true });
      if (
        !opened.isFile() ||
        opened.ino !== expected.ino ||
        opened.dev !== expected.dev
      )
        throw new Error(`Source changed while opening: ${file}`);
      return descriptor;
    } catch (error) {
      closeSync(descriptor);
      throw error;
    }
  } finally {
    for (const directory of directories.reverse()) closeSync(directory);
  }
}

/** Snapshot regular source files before formatting, keeping their root directory bound. */
export function readSourceEntries(root: number, files: string[]) {
  if (!files.length || new Set(files).size !== files.length)
    throw new Error("Empty or duplicate source paths");
  return [...files]
    .sort((left, right) =>
      Buffer.compare(Buffer.from(left), Buffer.from(right)),
    )
    .map((file) => {
      const descriptor = openSourceFile(root, file, false);
      try {
        const opened = fstatSync(descriptor, { bigint: true });
        const bytes = readFileSync(descriptor);
        const after = fstatSync(descriptor, { bigint: true });
        if (
          after.size !== opened.size ||
          after.mtimeNs !== opened.mtimeNs ||
          after.ctimeNs !== opened.ctimeNs
        )
          throw new Error(`Source changed while reading: ${file}`);
        return {
          path: file,
          ino: opened.ino,
          dev: opened.dev,
          sha256: createHash("sha256").update(bytes).digest("hex"),
        };
      } finally {
        closeSync(descriptor);
      }
    });
}
