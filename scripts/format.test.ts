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
import { formatFiles } from "./format.ts";
import { listSourcePaths } from "./source.ts";

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
  assert.deepEqual(await formatFiles(root, [file], false), [file]);
  assert.equal(readFileSync(path.join(root, file), "utf8"), original);
  await formatFiles(root, [file], true);
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

test("CLI covers new files and excludes deletions and ignored outputs", (t) => {
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
  writeFileSync(path.join(root, ".build/output.ts"), "invalid = ;\n");
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
    readFileSync(path.join(root, ".build/output.ts"), "utf8"),
    "invalid = ;\n",
  );
});

test("source enumeration excludes deleted children whose ancestor became a file", (t) => {
  const root = fixture(t);
  execFileSync("git", ["init", "--quiet", root]);
  mkdirSync(path.join(root, "old"));
  writeFileSync(path.join(root, "old/deleted.ts"), "const x = 1;\n");
  execFileSync("git", ["-C", root, "add", "old/deleted.ts"]);
  rmSync(path.join(root, "old"), { recursive: true });
  writeFileSync(path.join(root, "old"), "replacement\n");
  assert.ok(!listSourcePaths(root).includes("old/deleted.ts"));
});

function runRace(t: TestContext, scenario: string): void {
  const root = fixture(t);
  const script = `
    import assert from "node:assert/strict";
    import { execFileSync } from "node:child_process";
    import fs from "node:fs";
    import { syncBuiltinESMExports } from "node:module";
    import path from "node:path";
    import { formatFiles } from ${JSON.stringify(path.join(import.meta.dirname, "format.ts"))};
    const [base, scenario] = process.argv.slice(1);
    const root = path.join(base, "container/source");
    const outside = path.join(base, "outside/source");
    fs.mkdirSync(path.join(root, "nested"), { recursive: true });
    fs.mkdirSync(path.join(outside, "nested"), { recursive: true });
    fs.copyFileSync(path.join(base, ".prettierrc.json"), path.join(root, ".prettierrc.json"));
    const original = "const inside={x:1}\\n";
    const external = "const outside={y:2}\\n";
    const target = path.join(root, "nested/file.ts");
    const outsideFile = path.join(outside, "nested/file.ts");
    fs.writeFileSync(target, original);
    fs.writeFileSync(outsideFile, external);
    const saved = path.join(base, "saved");
    let injected = false;
    let leafOpens = 0;
    const descriptorsBefore = fs.readdirSync("/proc/self/fd").length;
    const realOpen = fs.openSync;
    const realLstat = fs.lstatSync;
    const parentSwap = () => {
      fs.renameSync(path.join(root, "nested"), saved);
      fs.symlinkSync(path.join(outside, "nested"), path.join(root, "nested"));
    };
    fs.lstatSync = function(file, ...args) {
      const result = realLstat.call(this, file, ...args);
      if (!injected && scenario === "snapshot-parent" && path.basename(String(file)) === "file.ts") {
        injected = true;
        parentSwap();
      }
      if (!injected && scenario === "initial-root" && String(file) === root) {
        injected = true;
        fs.renameSync(root, saved);
        fs.symlinkSync(outside, root);
      }
      if (!injected && scenario === "initial-root-ancestor" && String(file) === root) {
        injected = true;
        fs.renameSync(path.dirname(root), saved);
        fs.symlinkSync(path.dirname(outside), path.dirname(root));
      }
      return result;
    };
    fs.openSync = function(file, ...args) {
      if (scenario === "missing-proc" && String(file).startsWith("/proc/self/fd/")) {
        injected = true;
        throw Object.assign(new Error("Proc unavailable"), { code: "EACCES" });
      }
      if (path.basename(String(file)) === "file.ts") {
        leafOpens++;
        const phase = Number(scenario.split(":")[1]);
        if (!injected && scenario.startsWith("fifo:") && leafOpens === phase) {
          injected = true;
          fs.renameSync(target, path.join(base, "saved.ts"));
          execFileSync("mkfifo", [target]);
        }
        if (!injected && scenario === "reopen-parent" && leafOpens === 2) {
          injected = true;
          parentSwap();
        }
      }
      return realOpen.call(this, file, ...args);
    };
    syncBuiltinESMExports();
    let failed = false;
    try { await formatFiles(root, ["nested/file.ts"], !scenario.endsWith(":check")); }
    catch { failed = true; }
    assert.ok(injected, "The real filesystem replacement must run");
    assert.equal(fs.readFileSync(outsideFile, "utf8"), external, "Outside bytes must remain unchanged");
    if (scenario.startsWith("fifo:")) {
      assert.ok(failed, "FIFO replacement must fail");
      assert.equal(fs.readFileSync(path.join(base, "saved.ts"), "utf8"), original);
    } else if (scenario === "snapshot-parent") {
      assert.ok(failed, "The next traversal must reject the replaced parent");
      assert.equal(fs.readFileSync(path.join(saved, "file.ts"), "utf8"), original);
    } else if (scenario === "initial-root") {
      assert.ok(failed, "Root symlink substitution must fail");
      assert.equal(fs.readFileSync(path.join(saved, "nested/file.ts"), "utf8"), original);
    } else if (scenario === "initial-root-ancestor") {
      assert.ok(failed, "Root ancestor symlink substitution must fail");
      assert.equal(fs.readFileSync(path.join(saved, "source/nested/file.ts"), "utf8"), original);
    } else if (scenario === "missing-proc") {
      assert.ok(failed, "Missing proc access must fail without fallback");
      assert.equal(fs.readFileSync(target, "utf8"), original);
      assert.equal(fs.readdirSync("/proc/self/fd").length, descriptorsBefore, "Root descriptor must be closed on failure");
    } else {
      assert.ok(failed, "Write preparation must reject the replaced parent");
      assert.equal(fs.readFileSync(path.join(saved, "file.ts"), "utf8"), original);
    }
  `;
  const result = spawnSync(
    process.execPath,
    ["--input-type=module", "-e", script, root, scenario],
    {
      encoding: "utf8",
      timeout: 5_000,
    },
  );
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
}

test("initial snapshots cannot adopt an outside inode after a parent swap", (t) => {
  runRace(t, "snapshot-parent");
});

test("initial root substitution and later parent substitution fail without outside writes", (t) => {
  runRace(t, "initial-root");
  runRace(t, "initial-root-ancestor");
  runRace(t, "reopen-parent");
});

test("unavailable proc access fails without fallback or leaked directory descriptors", (t) => {
  runRace(t, "missing-proc");
});

test("FIFO substitution during snapshot, reopening and write preparation never blocks", (t) => {
  for (const scenario of [
    "fifo:1:check",
    "fifo:1:write",
    "fifo:2:check",
    "fifo:2:write",
    "fifo:3:write",
  ])
    runRace(t, scenario);
});
