import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { formatFiles, verifyGoFormat } from "../policy/format.ts";

test("format checking is read-only, ignores candidate overrides and shares repair rules", async (t) => {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-format-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const file = "name with [brackets].ts";
  const original = "const value={x:1}\r\n";
  writeFileSync(path.join(root, file), original);
  writeFileSync(path.join(root, ".prettierrc.json"), '{"semi":false}');
  writeFileSync(path.join(root, ".prettierignore"), "*\n");
  assert.deepEqual(await formatFiles(root, [file], "/unused", false), [file]);
  assert.equal(readFileSync(path.join(root, file), "utf8"), original);
  await formatFiles(root, [file], "/unused", true);
  assert.equal(
    readFileSync(path.join(root, file), "utf8"),
    "const value = { x: 1 };\n",
  );
  assert.deepEqual(await formatFiles(root, [file], "/unused", false), []);
  writeFileSync(path.join(root, file), "const = ;\n");
  await assert.rejects(formatFiles(root, [file], "/unused", false));
  const invalid = Buffer.from([0xff, 0x0a]);
  writeFileSync(path.join(root, file), invalid);
  await assert.rejects(formatFiles(root, [file], "/unused", true));
  assert.deepEqual(readFileSync(path.join(root, file)), invalid);
});

test("formatting rejects escapes, symlinks and an empty supported selection", async (t) => {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-format-paths-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(path.join(root, "real.ts"), "let x=1\n");
  symlinkSync("real.ts", path.join(root, "link.ts"));
  for (const files of [
    ["link.ts"],
    ["../outside.ts"],
    ["real.ts", "real.ts"],
    ["bun.lock"],
  ])
    await assert.rejects(formatFiles(root, files, "/unused", true));
  assert.equal(readFileSync(path.join(root, "real.ts"), "utf8"), "let x=1\n");
});

test("the actual Go formatter rejects invalid syntax and repairs Go source", async (t) => {
  const goBin = process.env.DESIGNS_GO_BIN;
  assert.ok(
    goBin,
    "Set DESIGNS_GO_BIN to the pinned Go toolchain for format tests",
  );
  const gofmt = verifyGoFormat(goBin);
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-format-go-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(
    path.join(root, "sample.go"),
    "package sample\nfunc Value()int{return 1}\n",
  );
  assert.deepEqual(await formatFiles(root, ["sample.go"], gofmt, false), [
    "sample.go",
  ]);
  await formatFiles(root, ["sample.go"], gofmt, true);
  assert.equal(
    readFileSync(path.join(root, "sample.go"), "utf8"),
    "package sample\n\nfunc Value() int { return 1 }\n",
  );
  assert.deepEqual(await formatFiles(root, ["sample.go"], gofmt, false), []);
  writeFileSync(path.join(root, "sample.go"), "package sample\nfunc {\n");
  await assert.rejects(formatFiles(root, ["sample.go"], gofmt, false));
});

test("the CLI covers nonignored new files and returns failure for formatting differences", (t) => {
  const goBin = process.env.DESIGNS_GO_BIN;
  assert.ok(goBin);
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-format-cli-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  execFileSync("git", ["init", "--quiet", root]);
  writeFileSync(path.join(root, ".gitignore"), "ignored.ts\n");
  writeFileSync(path.join(root, "ignored.ts"), "invalid = ;\n");
  writeFileSync(path.join(root, "new.ts"), "const x=1\n");
  const run = (mode: string) =>
    execFileSync(
      process.execPath,
      [
        path.resolve(import.meta.dirname, "../policy/format.ts"),
        mode,
        "--root",
        root,
        "--go-bin",
        goBin,
      ],
      { encoding: "utf8" },
    );
  assert.throws(
    () => run("--check"),
    (error: unknown) => (error as { status: number }).status === 1,
  );
  assert.match(run("--write"), /Formatted: new.ts/);
  assert.match(run("--check"), /Checked 1 source files/);
  assert.equal(
    readFileSync(path.join(root, "ignored.ts"), "utf8"),
    "invalid = ;\n",
  );
});
