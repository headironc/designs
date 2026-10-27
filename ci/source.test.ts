import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import type { TestContext } from "node:test";
import {
  canonicalDigest,
  listSourcePaths,
  readSourceEntries,
  readSourceIdentity,
  SourceIdentitySchema,
} from "./source.ts";
const fixtureEnvironment = () => ({
  PATH: process.env.PATH,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_NOSYSTEM: "1",
});

function fixture(t: TestContext): string {
  const directory = mkdtempSync(path.join(os.tmpdir(), "agents-source-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  execFileSync("git", ["init", "--quiet", directory], {
    env: fixtureEnvironment(),
  });
  return directory;
}

test("canonical digests sort object keys while preserving array order", () => {
  const expected = createHash("sha256")
    .update('{"a":[2,1],"b":{"c":3,"d":4}}')
    .digest("hex");
  assert.equal(canonicalDigest({ b: { d: 4, c: 3 }, a: [2, 1] }), expected);
  assert.notEqual(canonicalDigest({ a: [1, 2], b: { c: 3, d: 4 } }), expected);
  assert.throws(() => canonicalDigest({ missing: undefined }));
  assert.throws(() => canonicalDigest(Number.NaN));
});

test("source identity binds sorted paths, exact bytes and executable mode", (t) => {
  const directory = fixture(t);
  writeFileSync(path.join(directory, "b.txt"), "beta\n", { mode: 0o644 });
  writeFileSync(path.join(directory, "a.txt"), "alpha\n", { mode: 0o644 });
  const original = readSourceIdentity(directory, ["b.txt", "a.txt"]);
  assert.deepEqual(
    original.entries.map((entry) => entry.path),
    ["a.txt", "b.txt"],
  );
  assert.equal(
    original.entries[0]!.sha256,
    createHash("sha256").update("alpha\n").digest("hex"),
  );
  assert.equal(original.entries[0]!.bytes, 6);
  assert.equal(original.baseline, null);
  chmodSync(path.join(directory, "a.txt"), 0o755);
  assert.notEqual(
    readSourceIdentity(directory, ["a.txt", "b.txt"]).sha256,
    original.sha256,
  );
  writeFileSync(path.join(directory, "a.txt"), "alter\n");
  assert.notEqual(
    readSourceIdentity(directory, ["a.txt", "b.txt"]).sha256,
    original.sha256,
  );
});

test("snapshot entries outside Git use the identity file algorithm without a baseline", (t) => {
  const repository = fixture(t);
  const snapshot = mkdtempSync(path.join(os.tmpdir(), "agents-snapshot-"));
  t.after(() => rmSync(snapshot, { recursive: true, force: true }));
  for (const directory of [repository, snapshot]) {
    mkdirSync(path.join(directory, "nested"));
    writeFileSync(path.join(directory, "nested/tool"), "run\n");
    writeFileSync(path.join(directory, "guide.md"), "guide\n");
    chmodSync(path.join(directory, "nested/tool"), 0o755);
    chmodSync(path.join(directory, "guide.md"), 0o600);
  }
  assert.throws(() => readSourceIdentity(snapshot, ["guide.md"]));
  const entries = readSourceEntries(snapshot, ["nested/tool", "guide.md"]);
  assert.deepEqual(
    entries,
    readSourceIdentity(repository, ["guide.md", "nested/tool"]).entries,
  );
  assert.deepEqual(
    entries.map((entry) => [entry.path, entry.mode]),
    [
      ["guide.md", 0o600],
      ["nested/tool", 0o755],
    ],
  );
  symlinkSync("guide.md", path.join(snapshot, "linked"));
  assert.throws(() => readSourceEntries(snapshot, ["linked"]), /symlink/);
});

test("Git discovery includes new source and excludes deleted and ignored inputs", (t) => {
  const directory = fixture(t);
  writeFileSync(path.join(directory, ".gitignore"), "cache/\n");
  writeFileSync(path.join(directory, "old.md"), "old");
  execFileSync("git", ["add", "--", "old.md"], {
    cwd: directory,
    env: fixtureEnvironment(),
  });
  rmSync(path.join(directory, "old.md"));
  writeFileSync(path.join(directory, "new.md"), "new");
  mkdirSync(path.join(directory, "cache"));
  writeFileSync(path.join(directory, "cache/tool"), "tool");
  assert.deepEqual(listSourcePaths(directory), [".gitignore", "new.md"]);
  assert.equal(
    readSourceIdentity(directory, [...listSourcePaths(directory), "cache/tool"])
      .entries.length,
    3,
  );
});

test("Git discovery never executes a repository-local fsmonitor hook", (t) => {
  const directory = fixture(t);
  const git = (...args: string[]) =>
    execFileSync("git", args, {
      cwd: directory,
      env: fixtureEnvironment(),
    });
  writeFileSync(path.join(directory, "source.txt"), "source\n");
  git("add", "source.txt");
  const marker = path.join(directory, ".git/fsmonitor-invoked");
  writeFileSync(
    path.join(directory, ".git/fsmonitor-hook"),
    "#!/bin/sh\nprintf invoked > .git/fsmonitor-invoked\nprintf 'token\\000'\n",
    { mode: 0o755 },
  );
  git("config", "--local", "core.fsmonitor", ".git/fsmonitor-hook");
  // Prove the fixture actually executes without the trusted invocation override.
  git("ls-files", "--cached");
  assert.equal(existsSync(marker), true);
  rmSync(marker);
  assert.deepEqual(listSourcePaths(directory), ["source.txt"]);
  assert.equal(readSourceIdentity(directory, ["source.txt"]).entries.length, 1);
  assert.equal(existsSync(marker), false);
});

test("Git discovery rejects undecodable path bytes instead of dropping tracked source", (t) => {
  const directory = fixture(t);
  const environment = fixtureEnvironment();
  // The index exposes raw Git path bytes even when the filesystem rejects them.
  const blob = execFileSync("git", ["hash-object", "-w", "--stdin"], {
    cwd: directory,
    env: environment,
    input: "source\n",
    encoding: "utf8",
  }).trim();
  execFileSync("git", ["update-index", "-z", "--index-info"], {
    cwd: directory,
    env: environment,
    input: Buffer.concat([
      Buffer.from(`100644 ${blob}\t`),
      Buffer.from([0xff, 0]),
    ]),
  });
  assert.deepEqual(
    execFileSync("git", ["ls-files", "-z", "--cached"], {
      cwd: directory,
      env: environment,
    }),
    Buffer.from([0xff, 0]),
  );
  assert.throws(() => listSourcePaths(directory), /encoded data/);
});

test("Git discovery preserves a UTF-8 BOM at the start of a pathname", (t) => {
  const directory = fixture(t);
  const name = "\uFEFFsource.txt";
  writeFileSync(path.join(directory, name), "source\n");
  execFileSync("git", ["add", "--", name], {
    cwd: directory,
    env: fixtureEnvironment(),
  });
  assert.deepEqual(listSourcePaths(directory), [name]);
});

test("empty, duplicate, missing, escaping and special source members fail", (t) => {
  const directory = fixture(t);
  writeFileSync(path.join(directory, "file"), "contents");
  mkdirSync(path.join(directory, "folder"));
  for (const paths of [
    [],
    ["file", "file"],
    ["missing"],
    ["../outside"],
    ["./file"],
    ["folder"],
    [".git/config"],
  ]) {
    assert.throws(() => readSourceIdentity(directory, paths));
  }
  symlinkSync("file", path.join(directory, "link"));
  symlinkSync(directory, path.join(directory, "linked-directory"));
  assert.throws(() => readSourceIdentity(directory, ["link"]), /symlink/);
  assert.throws(
    () => readSourceIdentity(directory, ["linked-directory/file"]),
    /symlink/,
  );
});

test("schema rejects forged identity, unknown fields and noncanonical entries", (t) => {
  const directory = fixture(t);
  writeFileSync(path.join(directory, "a"), "a");
  writeFileSync(path.join(directory, "b"), "b");
  const value = readSourceIdentity(directory, ["a", "b"]);
  assert.equal(
    SourceIdentitySchema.safeParse({ ...value, sha256: "0".repeat(64) })
      .success,
    false,
  );
  assert.equal(
    SourceIdentitySchema.safeParse({ ...value, trusted: true }).success,
    false,
  );
  assert.equal(
    SourceIdentitySchema.safeParse({
      ...value,
      entries: [...value.entries].reverse(),
    }).success,
    false,
  );
});
