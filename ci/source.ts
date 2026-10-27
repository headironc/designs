import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import {
  constants,
  closeSync,
  fstatSync,
  lstatSync,
  openSync,
  readFileSync,
  realpathSync,
} from "node:fs";
import path from "node:path";
import { z } from "zod";

const digest = z.string().regex(/^[a-f0-9]{64}$/);
export const sourceGitEnvironment: NodeJS.ProcessEnv = {
  PATH: process.env.PATH,
  LC_ALL: "C",
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_NOSYSTEM: "1",
  GIT_OPTIONAL_LOCKS: "0",
};
const relativePath = z
  .string()
  .min(1)
  .refine(
    (value) =>
      !value.includes("\0") &&
      !value.includes("\\") &&
      !path.posix.isAbsolute(value) &&
      value
        .split("/")
        .every(
          (part) =>
            part !== "" && part !== "." && part !== ".." && part !== ".git",
        ),
    "Expected a normalized repository-relative path",
  );
export const SourceEntrySchema = z.strictObject({
  path: relativePath,
  type: z.literal("file"),
  mode: z.number().int().min(0).max(0o777),
  bytes: z.number().int().nonnegative(),
  sha256: digest,
});
export const SourceIdentitySchema = z
  .strictObject({
    version: z.literal(1),
    baseline: digest.or(z.string().regex(/^[a-f0-9]{40}$/)).nullable(),
    entries: z.array(SourceEntrySchema).min(1),
    sha256: digest,
  })
  .superRefine((value, context) => {
    const paths = value.entries.map((entry) => entry.path);
    if (
      new Set(paths).size !== paths.length ||
      paths.some(
        (item, index) =>
          index > 0 &&
          Buffer.compare(Buffer.from(paths[index - 1]!), Buffer.from(item)) >=
            0,
      )
    ) {
      context.addIssue({
        code: "custom",
        path: ["entries"],
        message: "Paths must be unique and byte-sorted",
      });
    }
    if (
      canonicalDigest({
        version: value.version,
        baseline: value.baseline,
        entries: value.entries,
      }) !== value.sha256
    ) {
      context.addIssue({
        code: "custom",
        path: ["sha256"],
        message: "Source identity digest mismatch",
      });
    }
  });
export type SourceEntry = z.infer<typeof SourceEntrySchema>;
export type SourceIdentity = z.infer<typeof SourceIdentitySchema>;

/** Hash JSON using sorted object keys, preserved array order and UTF-8 bytes. */
export function canonicalDigest(value: unknown): string {
  function normalize(item: unknown): unknown {
    if (item === null || typeof item === "string" || typeof item === "boolean")
      return item;
    if (typeof item === "number" && Number.isFinite(item)) return item;
    if (Array.isArray(item)) return item.map(normalize);
    if (
      typeof item === "object" &&
      item !== null &&
      Object.getPrototypeOf(item) === Object.prototype
    ) {
      return Object.fromEntries(
        Object.keys(item)
          .sort()
          .map((key) => [
            key,
            normalize((item as Record<string, unknown>)[key]),
          ]),
      );
    }
    throw new Error("Canonical input must contain only JSON values");
  }
  return createHash("sha256")
    .update(JSON.stringify(normalize(value)), "utf8")
    .digest("hex");
}

/** Enumerate current Git source members, including nonignored untracked files. */
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
      env: sourceGitEnvironment,
      maxBuffer: 16 * 1024 * 1024,
    },
  );
  const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
  const paths: string[] = [];
  let start = 0;
  for (
    let end = output.indexOf(0);
    end !== -1;
    end = output.indexOf(0, start)
  ) {
    paths.push(decoder.decode(output.subarray(start, end)));
    start = end + 1;
  }
  if (start !== output.length) throw new Error("Incomplete Git source listing");
  return [...new Set(paths.filter(Boolean))]
    .filter((file) => {
      relativePath.parse(file);
      try {
        lstatSync(path.join(root, file));
        return true;
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
        throw error;
      }
    })
    .sort((left, right) =>
      Buffer.compare(Buffer.from(left), Buffer.from(right)),
    );
}

/** Bind exactly the supplied regular files; reject links, escape and concurrent writes. */
export function readSourceIdentity(
  root: string,
  paths: string[],
): SourceIdentity {
  const entries = readSourceEntries(root, paths);
  const physicalRoot = realpathSync(root);
  let baseline: string | null = null;
  try {
    baseline = execFileSync(
      "git",
      ["-c", `safe.directory=${physicalRoot}`, "rev-parse", "--verify", "HEAD"],
      {
        cwd: physicalRoot,
        env: sourceGitEnvironment,
        encoding: "utf8",
        stdio: ["ignore", "pipe", "pipe"],
      },
    ).trim();
  } catch {
    execFileSync(
      "git",
      ["-c", `safe.directory=${physicalRoot}`, "rev-parse", "--git-dir"],
      {
        cwd: physicalRoot,
        env: sourceGitEnvironment,
        stdio: "pipe",
      },
    );
  }
  const body = { version: 1 as const, baseline, entries };
  return SourceIdentitySchema.parse({ ...body, sha256: canonicalDigest(body) });
}

/** Hash entries without Git, so persistent candidate snapshots use the same file algorithm. */
export function readSourceEntries(
  root: string,
  paths: string[],
): SourceEntry[] {
  const physicalRoot = realpathSync(root);
  if (paths.length === 0 || new Set(paths).size !== paths.length)
    throw new Error("Empty or duplicate source paths");
  return [...paths]
    .sort((left, right) =>
      Buffer.compare(Buffer.from(left), Buffer.from(right)),
    )
    .map((file) => {
      relativePath.parse(file);
      let current = physicalRoot;
      for (const part of file.split("/")) {
        current = path.join(current, part);
        if (lstatSync(current).isSymbolicLink())
          throw new Error(`Source symlink rejected: ${file}`);
      }
      const stat = lstatSync(current, { bigint: true });
      if (!stat.isFile())
        throw new Error(`Source must be a regular file: ${file}`);
      const descriptor = openSync(
        current,
        constants.O_RDONLY | constants.O_NOFOLLOW,
      );
      try {
        const opened = fstatSync(descriptor, { bigint: true });
        if (opened.ino !== stat.ino || opened.dev !== stat.dev)
          throw new Error(`Source changed while opening: ${file}`);
        const bytes = readFileSync(descriptor);
        const after = fstatSync(descriptor, { bigint: true });
        if (
          after.size !== opened.size ||
          after.mtimeNs !== opened.mtimeNs ||
          after.ctimeNs !== opened.ctimeNs
        ) {
          throw new Error(`Source changed while reading: ${file}`);
        }
        return {
          path: file,
          type: "file" as const,
          mode: Number(after.mode & 0o777n),
          bytes: bytes.length,
          sha256: createHash("sha256").update(bytes).digest("hex"),
        };
      } finally {
        closeSync(descriptor);
      }
    });
}
