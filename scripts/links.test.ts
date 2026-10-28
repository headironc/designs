import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import test, { type TestContext } from "node:test";
import { checkLinks, lychee, prepareLinks } from "./links.ts";

const repository = path.resolve(import.meta.dirname, "..");
const tool = path.join(repository, lychee.relativePath);

function fixture(t: TestContext): string {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-link-fixture-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  execFileSync("git", ["init", "--quiet", root]);
  return root;
}

function put(root: string, file: string, content: string | Buffer): void {
  const target = path.join(root, file);
  mkdirSync(path.dirname(target), { recursive: true });
  writeFileSync(target, content);
}

test("real Markdown headings, duplicate slugs, images, HTML and references; code is literal", async (t) => {
  const root = fixture(t);
  put(
    root,
    "README.md",
    `# Start
[Chinese](guide.md#中文标题)
[duplicate](guide.md#same-1)
![image](assets/pixel.svg)
<a href="guide.md#html-anchor">HTML link</a>
<img src="assets/pixel.svg">
[reference][guide]
[guide]: guide.md#same
[root](/assets/pixel.svg)
[directory](assets/)
\`[code](missing-inline.md)\`
\`\`\`md
[example](missing-code.md)
\`\`\`
[undefined-reference]
`,
  );
  put(
    root,
    "guide.md",
    '# 中文标题\n\n# Same\n\n# Same\n\n<a id="html-anchor"></a>\n',
  );
  put(root, "assets/pixel.svg", '<svg xmlns="http://www.w3.org/2000/svg"/>');
  const result = await checkLinks(root, tool);
  assert.equal(result.documents, 2);
  assert.deepEqual(result.failures, []);
});

test("all literal glob metacharacters, spaces and hidden paths are actually consumed", async (t) => {
  const root = fixture(t);
  const files = [
    "space name.md",
    "file[1].md",
    "star*.md",
    "question?.md",
    "brace{a,b}.md",
    ".hidden/中文.md",
    "trailing .md",
  ];
  for (const file of files)
    put(root, file, "# Heading\n[broken](missing.md)\n");
  const result = await checkLinks(root, tool);
  assert.equal(result.documents, files.length);
  assert.deepEqual(
    result.failures.map((failure) => failure.source).sort(),
    [...files].sort(),
  );
  for (const file of files) put(root, file, "# Heading\n[self](#heading)\n");
  assert.deepEqual((await checkLinks(root, tool)).failures, []);
});

test("missing files, anchors, images and HTML have repository paths and actual line numbers", async (t) => {
  const root = fixture(t);
  put(
    root,
    "README.md",
    '# Start\n[file](missing.md)\n[anchor](guide.md#gone)\n![image](missing.png)\n<a href="missing.html">HTML</a>\n',
  );
  put(root, "guide.md", "# Existing\n");
  const result = await checkLinks(root, tool);
  assert.deepEqual(
    result.failures.map((failure) => [failure.source, failure.line]).sort(),
    [
      ["README.md", 2],
      ["README.md", 3],
      ["README.md", 4],
      ["README.md", 5],
    ],
  );
  assert.match(
    result.failures.find((failure) => failure.line === 3)!.reason,
    /fragment/i,
  );
  assert.ok(
    result.failures.every(
      (failure) => !failure.target.includes("designs-links-"),
    ),
  );
});

test("Git source includes new and forced tracked targets, excludes ignored and deleted targets", async (t) => {
  const root = fixture(t);
  put(root, ".gitignore", "ignored/\n.cache/\n");
  put(root, "deleted.md", "# Deleted\n");
  put(root, "ignored/kept.md", "# Kept\n");
  execFileSync("git", ["add", "deleted.md"], { cwd: root });
  execFileSync("git", ["add", "-f", "ignored/kept.md"], { cwd: root });
  rmSync(path.join(root, "deleted.md"));
  put(root, "ignored/hidden.png", "exists only in live ignored source");
  put(root, "new.png", Buffer.from([0, 0xff, 0xfe, 0x0a]));
  put(
    root,
    "README.md",
    "[new](new.png)\n[kept](ignored/kept.md#kept)\n[ignored](ignored/hidden.png)\n[deleted](deleted.md)\n",
  );
  const result = await checkLinks(root, tool);
  assert.equal(result.documents, 2);
  assert.deepEqual(
    result.failures.map((failure) => failure.line).sort(),
    [3, 4],
  );
});

test("repository, parent, ignore and ambient settings cannot hide inputs or make network requests", async (t) => {
  const base = fixture(t);
  const root = path.join(base, "source");
  mkdirSync(root);
  execFileSync("git", ["init", "--quiet", root]);
  put(base, "lychee.toml", "invalid parent config\n");
  put(base, ".lycheeignore", ".*\n");
  put(root, "lychee/lychee.toml", "invalid user config\n");
  let requests = 0;
  const server = createServer((_request, response) => {
    requests++;
    response.end("unexpected network");
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(
    () =>
      new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      ),
  );
  const address = server.address();
  assert.ok(address && typeof address === "object");
  const marker = path.join(root, "preprocessed");
  put(
    root,
    "lychee.toml",
    `offline = false\nexclude = [".*"]\npreprocess = "touch ${marker}"\n`,
  );
  put(root, ".lycheeignore", ".*\n");
  put(root, ".ignore", "*.md\n");
  put(
    root,
    "README.md",
    `[external](http://127.0.0.1:${address.port}/external)\n[local](missing.md)\n`,
  );
  const keys = [
    "LYCHEE_OFFLINE",
    "LYCHEE_EXCLUDE",
    "GITHUB_TOKEN",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "ALL_PROXY",
    "GIT_DIR",
    "GIT_WORK_TREE",
    "XDG_CONFIG_HOME",
  ];
  const previous = keys.map((key) => process.env[key]);
  try {
    for (const key of keys)
      process.env[key] = key === "XDG_CONFIG_HOME" ? root : "untrusted-input";
    const result = await checkLinks(root, tool);
    assert.deepEqual(
      result.failures.map((failure) => failure.line),
      [2],
    );
    assert.equal(requests, 0);
    assert.equal(existsSync(marker), false);
  } finally {
    keys.forEach((key, index) => {
      if (previous[index] === undefined) delete process.env[key];
      else process.env[key] = previous[index];
    });
  }
});

test("escapes are attributed before checking outside files; encoded separators fail closed", async (t) => {
  const base = fixture(t);
  const root = path.join(base, "source");
  mkdirSync(root);
  execFileSync("git", ["init", "--quiet", root]);
  put(base, "outside.md", "# Outside\n");
  put(
    root,
    "README.md",
    `[relative](../outside.md)\n[absolute](file://${path.join(base, "outside.md")})\n[encoded](%2e%2e/outside.md)\n`,
  );
  const result = await checkLinks(root, tool);
  assert.equal(result.failures.length, 3);
  assert.ok(
    result.failures.every(
      (failure) =>
        failure.source === "README.md" && /escapes/.test(failure.reason),
    ),
  );
  put(root, "README.md", "[encoded](file:///tmp/a%2Fb)\n");
  await assert.rejects(checkLinks(root, tool));
  put(root, "README.md", "[host](file://otherhost/README.md)\n");
  await assert.rejects(checkLinks(root, tool));
});

test("source symlinks, symlinked parents, special files and ambiguous input names are rejected", async (t) => {
  const root = fixture(t);
  put(root, "README.md", "# Safe\n");
  symlinkSync("README.md", path.join(root, "link.md"));
  await assert.rejects(checkLinks(root, tool), /regular file/);
  rmSync(path.join(root, "link.md"));
  put(root, "real/file.md", "# Safe\n");
  execFileSync("git", ["add", "real/file.md"], { cwd: root });
  renameSync(path.join(root, "real"), path.join(root, "saved"));
  symlinkSync("saved", path.join(root, "real"));
  await assert.rejects(checkLinks(root, tool));
  rmSync(path.join(root, "real"));
  execFileSync("git", ["rm", "--cached", "--quiet", "real/file.md"], {
    cwd: root,
  });
  put(root, "newline\nname.md", "# Safe\n");
  await assert.rejects(checkLinks(root, tool), /control character/);
  rmSync(path.join(root, "newline\nname.md"));
  put(root, "pipe.md", "# Safe\n");
  execFileSync("git", ["add", "pipe.md"], { cwd: root });
  rmSync(path.join(root, "pipe.md"));
  execFileSync("mkfifo", [path.join(root, "pipe.md")]);
  await assert.rejects(checkLinks(root, tool), /regular file/);
});

test("empty input, absent tools and a version-only substitute cannot pass", async (t) => {
  const root = fixture(t);
  put(root, "only.txt", "no Markdown");
  await assert.rejects(checkLinks(root, tool), /No Markdown/);
  put(root, "README.md", "# Safe\n");
  await assert.rejects(checkLinks(root, path.join(root, "missing")));
  const fake = path.join(root, "fake");
  put(root, "fake", "#!/bin/sh\nprintf 'lychee 0.24.2\\n'\n");
  chmodSync(fake, 0o700);
  await assert.rejects(checkLinks(root, fake), /wrong lychee size/);
  put(root, "fake", Buffer.alloc(lychee.binarySize));
  await assert.rejects(checkLinks(root, fake), /checksum/);
  symlinkSync(tool, path.join(root, "linked-tool"));
  await assert.rejects(
    checkLinks(root, path.join(root, "linked-tool")),
    /regular file/,
  );
  const cli = spawnSync(
    process.execPath,
    [path.join(repository, "scripts/links.ts"), "README.md"],
    { encoding: "utf8" },
  );
  assert.equal(cli.status, 2);
  assert.match(cli.stderr, /Usage:/);
});

test("the actual CLI distinguishes success, broken links and missing preparation", (t) => {
  const root = fixture(t);
  for (const file of [
    "scripts/links.ts",
    "scripts/source.ts",
    lychee.relativePath,
  ]) {
    const destination = path.join(root, file);
    mkdirSync(path.dirname(destination), { recursive: true });
    copyFileSync(path.join(repository, file), destination);
  }
  put(root, "package.json", '{"type":"module"}\n');
  put(root, ".gitignore", ".cache/\n");
  put(root, "README.md", "# Heading\n[bad](missing.md)\n");
  const run = () =>
    spawnSync(process.execPath, [path.join(root, "scripts/links.ts")], {
      cwd: root,
      encoding: "utf8",
      timeout: 10_000,
    });
  const broken = run();
  assert.equal(broken.status, 1);
  assert.match(broken.stderr, /README.md:2:.*missing.md/);
  put(root, "README.md", "# Heading\n[good](#heading)\n");
  const success = run();
  assert.equal(success.status, 0);
  assert.match(success.stdout, /external URLs excluded \(offline\)/);
  rmSync(path.join(root, lychee.relativePath));
  const absent = run();
  assert.equal(absent.status, 2);
  assert.equal(existsSync(path.join(root, lychee.relativePath)), false);
});

test("live edits, file replacement, source-set changes and root replacement invalidate the snapshot", async (t) => {
  for (const mutation of [
    "edit",
    "replace",
    "add",
    "delete",
    "root",
  ] as const) {
    const root = fixture(t);
    put(root, "README.md", "# Safe\n");
    const pending = checkLinks(root, tool);
    // checkLinks has copied all source before awaiting the real binary's version.
    if (mutation === "edit") put(root, "README.md", "# Changed\n");
    if (mutation === "replace") {
      rmSync(path.join(root, "README.md"));
      put(root, "README.md", "# Safe\n");
    }
    if (mutation === "add") put(root, "new.png", "new source target");
    if (mutation === "delete") rmSync(path.join(root, "README.md"));
    if (mutation === "root") {
      const moved = `${root}-moved`;
      renameSync(root, moved);
      t.after(() => rmSync(moved, { recursive: true, force: true }));
      mkdirSync(root);
      execFileSync("git", ["init", "--quiet", root]);
      put(root, "README.md", "# Safe\n");
    }
    await assert.rejects(pending, /Source.*changed/);
  }
});

test("failed downloads preserve the existing binary and clean staging directories", async (t) => {
  const root = fixture(t);
  const installed = path.join(root, lychee.relativePath);
  mkdirSync(path.dirname(installed), { recursive: true });
  copyFileSync(tool, installed);
  const original = readFileSync(installed);
  for (const response of [
    new Response("failure", { status: 503 }),
    new Response("short"),
    new Response(Buffer.alloc(lychee.archiveSize)),
    new Response(Buffer.alloc(lychee.archiveSize + 1)),
  ]) {
    const request: typeof fetch = async (input, init) => {
      assert.equal(input, lychee.url);
      assert.ok(init?.signal);
      return response;
    };
    await assert.rejects(prepareLinks(root, request));
    assert.deepEqual(readFileSync(installed), original);
    assert.deepEqual(readdirSync(path.dirname(installed)), ["lychee"]);
  }
  const blocked = fixture(t);
  symlinkSync(root, path.join(blocked, ".cache"));
  let fetched = false;
  await assert.rejects(
    prepareLinks(blocked, async () => {
      fetched = true;
      throw new Error("not reached");
    }),
  );
  assert.equal(fetched, false);
});

test("startup and completed-check failures close descriptors and remove private snapshots", async (t) => {
  const root = fixture(t);
  put(root, "README.md", "# Safe\n");
  const temporaryBase = path.join(root, "temporary");
  mkdirSync(temporaryBase);
  put(root, ".gitignore", "temporary/\n");
  const previous = process.env.TMPDIR;
  const descriptors = () => readdirSync("/proc/self/fd").length;
  try {
    process.env.TMPDIR = path.join(temporaryBase, "missing");
    const count = descriptors();
    for (let attempt = 0; attempt < 3; attempt++)
      await assert.rejects(checkLinks(root, tool), /ENOENT/);
    assert.equal(descriptors(), count);
    assert.deepEqual(readdirSync(temporaryBase), []);
    process.env.TMPDIR = temporaryBase;
    assert.deepEqual((await checkLinks(root, tool)).failures, []);
    assert.deepEqual(readdirSync(temporaryBase), []);
    put(root, "README.md", "[unsupported](file:///tmp/a%2Fb)\n");
    await assert.rejects(checkLinks(root, tool));
    assert.deepEqual(readdirSync(temporaryBase), []);
  } finally {
    if (previous === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = previous;
  }
  const directory = path.join(root, ".cache/designs-links");
  mkdirSync(directory, { recursive: true });
  chmodSync(directory, 0o500);
  const count = descriptors();
  try {
    await assert.rejects(
      prepareLinks(root, async () => {
        throw new Error("download must not start");
      }),
      /EACCES/,
    );
    assert.equal(descriptors(), count);
    assert.deepEqual(readdirSync(directory), []);
  } finally {
    chmodSync(directory, 0o700);
  }
});

test("download cancellation errors retain the original bounded-size failure", async (t) => {
  const root = fixture(t);
  const request: typeof fetch = async () =>
    new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(Buffer.alloc(lychee.archiveSize + 1));
        },
        cancel() {
          throw new Error("stream cancellation failed");
        },
      }),
    );
  await assert.rejects(prepareLinks(root, request), (error: unknown) => {
    assert.ok(error instanceof AggregateError);
    assert.match(String(error.errors[0]), /exceeds pinned size/);
    assert.match(String(error.errors[1]), /stream cancellation failed/);
    return true;
  });
  assert.deepEqual(readdirSync(path.join(root, ".cache/designs-links")), []);
});

test("replaced preparation paths are preserved and do not hide the download error", async (t) => {
  const root = fixture(t);
  const outside = fixture(t);
  put(outside, "sentinel", "preserve unrelated work");
  const directory = path.join(root, ".cache/designs-links");
  const request: typeof fetch = async () => {
    const staging = path.join(
      directory,
      readdirSync(directory).find((file) => file.startsWith("prepare-"))!,
    );
    renameSync(staging, `${staging}-moved`);
    symlinkSync(outside, staging);
    throw new Error("download connection failed");
  };
  await assert.rejects(prepareLinks(root, request), (error: unknown) => {
    assert.ok(error instanceof AggregateError);
    assert.match(String(error.errors[0]), /download connection failed/);
    assert.match(String(error.errors[1]), /staging path changed/);
    return true;
  });
  assert.deepEqual(readdirSync(outside), [".git", "sentinel"]);
  assert.equal(
    readFileSync(path.join(outside, "sentinel"), "utf8"),
    "preserve unrelated work",
  );
  assert.ok(readdirSync(directory).some((file) => file.endsWith("-moved")));
});
