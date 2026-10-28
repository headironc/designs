import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  copyFileSync,
  linkSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import test, { type TestContext } from "node:test";
import { formatFiles, verifyGoFormat } from "./format.ts";

const repository = path.resolve(import.meta.dirname, "..");

function fixture(t: TestContext): string {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-format-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  copyFileSync(
    path.join(repository, ".prettierrc.json"),
    path.join(root, ".prettierrc.json"),
  );
  return root;
}

test("checking is read-only, repair is idempotent and repository settings can change", async (t) => {
  const base = fixture(t);
  const root = path.join(base, "repository");
  mkdirSync(root);
  copyFileSync(
    path.join(base, ".prettierrc.json"),
    path.join(root, ".prettierrc.json"),
  );
  writeFileSync(path.join(base, ".prettierrc.json"), '{"semi":false}\n');
  const file = "name with [brackets].ts";
  const original = "const value={x:1}\r\n";
  writeFileSync(path.join(root, file), original);
  // Neither ignored patterns nor a config in the parent takes ownership of this file.
  writeFileSync(path.join(root, ".prettierignore"), "*\n");
  assert.deepEqual(await formatFiles(root, [file], false, "/missing-go"), [
    file,
  ]);
  assert.equal(readFileSync(path.join(root, file), "utf8"), original);
  await formatFiles(root, [file], true, "/missing-go");
  assert.equal(
    readFileSync(path.join(root, file), "utf8"),
    "const value = { x: 1 };\n",
  );
  assert.deepEqual(await formatFiles(root, [file], false), []);
  assert.deepEqual(await formatFiles(root, [file], true), []);
  writeFileSync(path.join(root, ".prettierrc.json"), '{"semi":false}\n');
  await formatFiles(root, [file], true);
  assert.equal(
    readFileSync(path.join(root, file), "utf8"),
    "const value = { x: 1 }\n",
  );

  const syntax = "const = ;\n";
  writeFileSync(path.join(root, file), syntax);
  await assert.rejects(
    formatFiles(root, [file], true),
    /name with \[brackets\].ts/,
  );
  assert.equal(readFileSync(path.join(root, file), "utf8"), syntax);
  const invalid = Buffer.from([0xff, 0x0a]);
  writeFileSync(path.join(root, file), invalid);
  await assert.rejects(
    formatFiles(root, [file], true),
    /name with \[brackets\].ts/,
  );
  assert.deepEqual(readFileSync(path.join(root, file)), invalid);
});

test("formatting rejects escapes, symlinks, duplicates and an empty supported selection", async (t) => {
  const root = fixture(t);
  writeFileSync(path.join(root, "real.ts"), "let x=1\n");
  symlinkSync("real.ts", path.join(root, "link.ts"));
  mkdirSync(path.join(root, "nested"));
  writeFileSync(path.join(root, "nested/file.ts"), "let x=1\n");
  symlinkSync("nested", path.join(root, "linked-parent"));
  mkdirSync(path.join(root, "directory.ts"));
  for (const files of [
    ["link.ts"],
    ["linked-parent/file.ts"],
    ["directory.ts"],
    ["../outside.ts"],
    [path.join(root, "real.ts")],
    ["real.ts", "real.ts"],
    ["bun.lock"],
  ]) {
    for (const write of [false, true])
      await assert.rejects(formatFiles(root, files, write));
  }
  assert.equal(readFileSync(path.join(root, "real.ts"), "utf8"), "let x=1\n");
});

test("concurrent file and parent replacement cannot redirect a format write", async (t) => {
  for (const replaceParent of [false, true]) {
    const base = fixture(t);
    const root = path.join(base, "source");
    const directory = path.join(root, "nested");
    const outside = path.join(base, "outside");
    mkdirSync(directory, { recursive: true });
    mkdirSync(outside);
    copyFileSync(
      path.join(base, ".prettierrc.json"),
      path.join(root, ".prettierrc.json"),
    );
    const original = "const value={x:1}\n";
    writeFileSync(path.join(directory, "file.ts"), original);
    writeFileSync(
      path.join(outside, "file.ts"),
      "outside must remain unchanged\n",
    );
    // The first await occurs after opening and reading this file.
    const pending = formatFiles(root, ["nested/file.ts"], true);
    if (replaceParent) {
      renameSync(directory, path.join(root, "saved"));
      symlinkSync(outside, directory);
    } else {
      renameSync(
        path.join(directory, "file.ts"),
        path.join(directory, "saved.ts"),
      );
      symlinkSync(
        path.join(outside, "file.ts"),
        path.join(directory, "file.ts"),
      );
    }
    await assert.rejects(pending, /Source changed during formatting/);
    assert.equal(
      readFileSync(path.join(outside, "file.ts"), "utf8"),
      "outside must remain unchanged\n",
    );
    assert.equal(
      readFileSync(
        replaceParent
          ? path.join(root, "saved/file.ts")
          : path.join(directory, "saved.ts"),
        "utf8",
      ),
      original,
    );
  }
});

test("repair preserves concurrent edits and refuses hardlinked files", async (t) => {
  const root = fixture(t);
  const file = path.join(root, "file.ts");
  writeFileSync(file, "const value=1\n");
  const pending = formatFiles(root, ["file.ts"], true);
  const edited = "const value = 200;\n";
  writeFileSync(file, edited);
  await assert.rejects(pending, /Source changed during formatting/);
  assert.equal(readFileSync(file, "utf8"), edited);
  linkSync(file, path.join(root, "linked.ts"));
  await assert.rejects(formatFiles(root, ["linked.ts"], true), /is linked/);
  assert.equal(readFileSync(file, "utf8"), edited);
});

test("the actual Go formatter rejects invalid syntax and repairs Go source", async (t) => {
  verifyGoFormat();
  const root = fixture(t);
  writeFileSync(
    path.join(root, "sample.go"),
    "package sample\nfunc Value()int{return 1}\n",
  );
  assert.deepEqual(await formatFiles(root, ["sample.go"], false), [
    "sample.go",
  ]);
  await formatFiles(root, ["sample.go"], true);
  assert.equal(
    readFileSync(path.join(root, "sample.go"), "utf8"),
    "package sample\n\nfunc Value() int { return 1 }\n",
  );
  assert.deepEqual(await formatFiles(root, ["sample.go"], false), []);
  const invalid = "package sample\nfunc {\n";
  writeFileSync(path.join(root, "sample.go"), invalid);
  await assert.rejects(formatFiles(root, ["sample.go"], true), /sample.go/);
  assert.equal(readFileSync(path.join(root, "sample.go"), "utf8"), invalid);
});

test("CLI covers new files, excludes deletions and ignored outputs, and needs no Go for TS", (t) => {
  const root = fixture(t);
  mkdirSync(path.join(root, "scripts"));
  for (const file of ["format.ts", "source.ts"])
    copyFileSync(
      path.join(repository, "scripts", file),
      path.join(root, "scripts", file),
    );
  writeFileSync(path.join(root, "package.json"), '{"type":"module"}\n');
  symlinkSync(
    path.join(repository, "node_modules"),
    path.join(root, "node_modules"),
  );
  execFileSync("git", ["init", "--quiet", root]);
  writeFileSync(
    path.join(root, ".gitignore"),
    "ignored.ts\n.build/\nnode_modules/\n",
  );
  writeFileSync(path.join(root, "deleted.ts"), "invalid = ;\n");
  execFileSync("git", ["-C", root, "add", ".gitignore", "deleted.ts"]);
  rmSync(path.join(root, "deleted.ts"));
  writeFileSync(path.join(root, "ignored.ts"), "invalid = ;\n");
  mkdirSync(path.join(root, ".build"));
  writeFileSync(path.join(root, ".build/output.go"), "invalid = ;\n");
  const literal = "new with [brackets].ts";
  writeFileSync(path.join(root, literal), "const x=1\n");
  const run = (...args: string[]) =>
    spawnSync(
      process.execPath,
      [path.join(root, "scripts/format.ts"), ...args],
      {
        cwd: os.tmpdir(),
        encoding: "utf8",
        env: {
          ...process.env,
          DESIGNS_GO_BIN: "/missing-go",
          GIT_DIR: "/unrelated",
          GIT_WORK_TREE: "/unrelated",
        },
        timeout: 30_000,
      },
    );
  const failure = run("--check");
  assert.equal(failure.status, 1, failure.stderr);
  assert.match(failure.stdout, /Unformatted: new with \[brackets\].ts/);
  assert.equal(readFileSync(path.join(root, literal), "utf8"), "const x=1\n");
  const repair = run("--write");
  assert.equal(repair.status, 0, repair.stderr);
  assert.match(repair.stdout, /Formatted: new with \[brackets\].ts/);
  assert.equal(run("--check").status, 0);
  assert.equal(run("--check", literal).status, 0);
  assert.equal(run("--check", "../escape.ts").status, 2);
  assert.equal(run("--check", "bun.lock").status, 2);
  const syntax = "const = ;\n";
  writeFileSync(path.join(root, literal), syntax);
  const parseFailure = run("--write", literal);
  assert.equal(parseFailure.status, 2);
  assert.match(parseFailure.stderr, /new with \[brackets\].ts/);
  assert.equal(readFileSync(path.join(root, literal), "utf8"), syntax);
  writeFileSync(path.join(root, literal), "const x = 1;\n");
  writeFileSync(path.join(root, ".prettierrc.json"), '{"semi":false}\n');
  assert.equal(run("--check", literal).status, 1);
  assert.equal(run("--write", literal).status, 0);
  assert.equal(readFileSync(path.join(root, literal), "utf8"), "const x = 1\n");
  writeFileSync(path.join(root, ".prettierrc.json"), "{invalid\n");
  assert.equal(run("--check", literal).status, 2);
  copyFileSync(
    path.join(repository, ".prettierrc.json"),
    path.join(root, ".prettierrc.json"),
  );
  writeFileSync(path.join(root, "sample.go"), "package sample\n");
  const missingGo = run("--check", "sample.go");
  assert.equal(missingGo.status, 2);
  assert.match(missingGo.stderr, /missing-go/);
  rmSync(path.join(root, "sample.go"));
  writeFileSync(
    Buffer.concat([
      Buffer.from(root + path.sep),
      Buffer.from([0xff]),
      Buffer.from(".ts"),
    ]),
    "const x = 1;\n",
  );
  assert.equal(run("--check").status, 2);
  assert.equal(
    readFileSync(path.join(root, "ignored.ts"), "utf8"),
    "invalid = ;\n",
  );
  assert.equal(
    readFileSync(path.join(root, ".build/output.go"), "utf8"),
    "invalid = ;\n",
  );
});
