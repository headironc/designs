import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import test from "node:test";
import { unpackShared } from "./bootstrap.mjs";

const digest = (file) =>
  createHash("sha256").update(readFileSync(file)).digest("hex");
test("native bundle rejects substitution, missing lock, wrong version, extra refs and escaping members", (t) => {
  const root = mkdtempSync("/tmp/designs-distribution-");
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const source = path.join(root, "source");
  mkdirSync(source);
  const git = (...args) =>
    execFileSync("/usr/bin/git", args, {
      cwd: source,
      encoding: "utf8",
      env: {
        PATH: "/usr/bin:/bin",
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
      },
    }).trim();
  git("init", "--quiet");
  writeFileSync(path.join(source, "bun.lock"), '{"lockfileVersion":1}\n');
  writeFileSync(
    path.join(source, "main.mjs"),
    "throw Error('must not run during extraction')\n",
  );
  git("add", ".");
  git(
    "-c",
    "user.name=Fixture",
    "-c",
    "user.email=fixture@example.invalid",
    "commit",
    "--quiet",
    "--no-gpg-sign",
    "-m",
    "fixture",
  );
  const commit = git("rev-parse", "HEAD"),
    tree = git("rev-parse", "HEAD^{tree}");
  const bundle = path.join(root, "source.bundle");
  git("bundle", "create", bundle, "HEAD");
  const expected = {
    commit,
    tree,
    lockPath: "bun.lock",
    lock: digest(path.join(source, "bun.lock")),
    distribution: digest(bundle),
  };
  const output = unpackShared(bundle, expected, path.join(root, "good"));
  assert.equal(
    readFileSync(path.join(output, "main.mjs"), "utf8"),
    readFileSync(path.join(source, "main.mjs"), "utf8"),
  );
  assert.throws(
    () =>
      unpackShared(
        bundle,
        { ...expected, distribution: "0".repeat(64) },
        path.join(root, "swapped"),
      ),
    /substituted/,
  );
  assert.throws(
    () =>
      unpackShared(
        bundle,
        { ...expected, commit: "1".repeat(40) },
        path.join(root, "version"),
      ),
    /pinned source/,
  );
  assert.throws(
    () =>
      unpackShared(
        bundle,
        { ...expected, lock: "2".repeat(64) },
        path.join(root, "lock"),
      ),
    /lock mismatch/,
  );
  assert.throws(
    () => unpackShared(bundle, expected, path.join(root, "good")),
    /EEXIST/,
  );
  git("branch", "extra");
  const extra = path.join(root, "extra.bundle");
  git("bundle", "create", extra, "--all");
  assert.throws(
    () =>
      unpackShared(
        extra,
        { ...expected, distribution: digest(extra) },
        path.join(root, "extra"),
      ),
    /exactly/,
  );
  git("rm", "bun.lock");
  git(
    "-c",
    "user.name=Fixture",
    "-c",
    "user.email=fixture@example.invalid",
    "commit",
    "--quiet",
    "--no-gpg-sign",
    "-m",
    "missing lock",
  );
  const lockless = path.join(root, "lockless.bundle");
  git("bundle", "create", lockless, "HEAD");
  const missing = path.join(root, "missing-lock");
  assert.throws(
    () =>
      unpackShared(
        lockless,
        {
          ...expected,
          commit: git("rev-parse", "HEAD"),
          tree: git("rev-parse", "HEAD^{tree}"),
          distribution: digest(lockless),
        },
        missing,
      ),
    { code: "ENOENT" },
  );
  assert.equal(existsSync(missing), false);
  writeFileSync(path.join(source, "bun.lock"), '{"lockfileVersion":1}\n');
  symlinkSync("/etc/passwd", path.join(source, "escape"));
  git("add", "bun.lock", "escape");
  git(
    "-c",
    "user.name=Fixture",
    "-c",
    "user.email=fixture@example.invalid",
    "commit",
    "--quiet",
    "--no-gpg-sign",
    "-m",
    "link",
  );
  const unsafe = path.join(root, "unsafe.bundle");
  git("bundle", "create", unsafe, "HEAD");
  assert.throws(
    () =>
      unpackShared(
        unsafe,
        {
          ...expected,
          commit: git("rev-parse", "HEAD"),
          tree: git("rev-parse", "HEAD^{tree}"),
          distribution: digest(unsafe),
        },
        path.join(root, "unsafe"),
      ),
    /symlink/,
  );
});
