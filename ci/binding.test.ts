import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  cpSync,
  mkdirSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { fileDigest } from "./binding.ts";
import type { SourcePin } from "./contract.ts";

test("candidate snapshots preserve executable meaning across private and protected modes", async (t) => {
  const root = mkdtempSync("/tmp/designs-candidate-binding-");
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const policy = path.join(root, "policy"),
    shared = path.join(policy, ".shared");
  mkdirSync(shared, { recursive: true });
  const git = (cwd: string, ...args: string[]) =>
    execFileSync(
      "git",
      [
        "-c",
        "user.name=Fixture",
        "-c",
        "user.email=fixture@example.invalid",
        "-c",
        "commit.gpgsign=false",
        ...args,
      ],
      {
        cwd,
        encoding: "utf8",
        env: {
          PATH: process.env.PATH,
          GIT_CONFIG_GLOBAL: "/dev/null",
          GIT_CONFIG_NOSYSTEM: "1",
        },
      },
    ).trim();
  const commit = (directory: string) => {
    git(directory, "init", "--quiet");
    git(directory, "add", ".");
    git(directory, "commit", "--quiet", "-m", "fixture");
  };
  writeFileSync(path.join(policy, ".gitignore"), ".shared/\n");
  writeFileSync(path.join(policy, "README.md"), "Candidate document\n");
  writeFileSync(path.join(policy, "bun.lock"), "fixture\n");
  commit(policy);
  const source = path.resolve(import.meta.dirname, "..");
  mkdirSync(path.join(shared, "ci"));
  for (const file of [
    "binding.ts",
    "contract.ts",
    "source.ts",
    "artifact.ts",
    "http.ts",
  ])
    copyFileSync(path.join(source, "ci", file), path.join(shared, "ci", file));
  for (const file of ["package.json", "bun.lock"])
    copyFileSync(path.join(source, file), path.join(shared, file));
  writeFileSync(path.join(shared, ".gitignore"), "node_modules/\n");
  commit(shared);
  cpSync(path.join(source, "node_modules"), path.join(shared, "node_modules"), {
    recursive: true,
    verbatimSymlinks: true,
  });
  const bundle = path.join(root, "shared.bundle");
  git(shared, "bundle", "create", bundle, "HEAD");
  const candidate = path.join(root, "candidate"),
    snapshot = path.join(root, "snapshot");
  git(root, "clone", "--quiet", "--no-hardlinks", policy, candidate);
  git(root, "clone", "--quiet", "--no-hardlinks", policy, snapshot);
  chmodSync(path.join(candidate, "README.md"), 0o600);
  const pin = (directory: string): SourcePin => ({
    commit: git(directory, "rev-parse", "HEAD"),
    tree: git(directory, "rev-parse", "HEAD^{tree}"),
    lockPath: "bun.lock",
    lock: fileDigest(path.join(directory, "bun.lock")),
  });
  const p = pin(policy),
    executable = realpathSync(process.execPath);
  const { BoundInputs } = (await import(
    pathToFileURL(path.join(shared, "ci/binding.ts")).href
  )) as typeof import("./binding.ts");
  const inputs = BoundInputs.verify(
    { policy: p, shared: pin(shared), distribution: fileDigest(bundle) },
    { event: "push", commit: p.commit, tree: p.tree },
    {
      policy,
      shared,
      candidate,
      tools: [
        {
          name: "node",
          executable,
          sha256: fileDigest(executable),
          versionArgs: ["--version"],
          version: process.version,
        },
      ],
    },
    bundle,
    {
      repository: "fixture/designs",
      repositoryId: "1",
      target: "main",
      workflow: ".github/workflows/docs.yml",
    },
  );
  assert.equal(
    inputs.source.entries.find((entry) => entry.path === "README.md")?.mode,
    0o600,
  );
  assert.equal(inputs.verifyCandidate(candidate), candidate);
  assert.equal(inputs.verifyCandidate(snapshot), snapshot);
  chmodSync(path.join(candidate, "README.md"), 0o444);
  assert.equal(inputs.verifyCandidate(candidate), candidate);
  git(candidate, "config", "core.filemode", "false");
  chmodSync(path.join(candidate, "README.md"), 0o755);
  assert.equal(git(candidate, "status", "--porcelain"), "");
  assert.throws(() => inputs.verifyCandidate(candidate), /source differs/);
  chmodSync(path.join(candidate, "README.md"), 0o644);
  writeFileSync(path.join(candidate, "README.md"), "Changed candidate\n");
  assert.throws(() => inputs.verifyCandidate(candidate), /does not match/);
});
