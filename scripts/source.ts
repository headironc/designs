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
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
      throw error;
    }
  });
}

/** Snapshot regular source files before formatting, without following links. */
export function readSourceEntries(root: string, files: string[]) {
  const physicalRoot = realpathSync(root);
  if (!files.length || new Set(files).size !== files.length)
    throw new Error("Empty or duplicate source paths");
  return [...files]
    .sort((left, right) =>
      Buffer.compare(Buffer.from(left), Buffer.from(right)),
    )
    .map((file) => {
      validatePath(file);
      let current = physicalRoot;
      for (const part of file.split("/")) {
        current = path.join(current, part);
        if (lstatSync(current).isSymbolicLink())
          throw new Error(`Source symlink rejected: ${file}`);
      }
      const expected = lstatSync(current, { bigint: true });
      if (!expected.isFile())
        throw new Error(`Source must be a regular file: ${file}`);
      const descriptor = openSync(
        current,
        constants.O_RDONLY | constants.O_NOFOLLOW,
      );
      try {
        const opened = fstatSync(descriptor, { bigint: true });
        if (
          !opened.isFile() ||
          opened.ino !== expected.ino ||
          opened.dev !== expected.dev
        )
          throw new Error(`Source changed while opening: ${file}`);
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
