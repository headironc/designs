import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  copyFileSync,
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:http";
import os from "node:os";
import path from "node:path";
import test, { type TestContext } from "node:test";
import { gzipSync } from "node:zlib";
import {
  checkVulnerabilities,
  describeFailure,
  vulnerabilityEnvironment,
  vulnTool,
} from "./go-vuln.ts";
import { CommandFailure, runCommand } from "./go-check.ts";
import {
  evaluateLXD,
  hashJSON,
  inspectPackages,
  lxdIDs,
  lxdModule,
  lxdPackages,
  parseScan,
  runnerInputHash,
  scopeIdentity,
  verifyNativeStatus,
  type ScanIdentity,
} from "./go-vuln-report.ts";

const repository = path.resolve(import.meta.dirname, "..");
const tool = path.join(repository, vulnTool.cache, "govulncheck");
const goBin =
  process.env.DESIGNS_GO_BIN ??
  path.dirname(execFileSync("which", ["go"], { encoding: "utf8" }).trim());
const go = path.join(goBin, "go");
const synthetic = {
  schema_version: "1.3.1",
  id: "GO-2099-0001",
  modified: "2026-10-28T00:00:00Z",
  published: "2026-10-28T00:00:00Z",
  summary: "Synthetic acceptance finding",
  details: "Invented scanner fixture; not a real vulnerability.",
  database_specific: { url: "https://example.invalid/synthetic" },
  affected: [
    {
      package: { name: "stdlib", ecosystem: "Go" },
      ranges: [
        { type: "SEMVER", events: [{ introduced: "0" }, { fixed: "99.0.0" }] },
      ],
      ecosystem_specific: { imports: [{ path: "fmt", symbols: ["Println"] }] },
    },
  ],
};

function fixture(t: TestContext) {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-vuln-test-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  execFileSync("git", ["init", "--quiet", root]);
  mkdirSync(path.join(root, "runner"));
  mkdirSync(path.join(root, "scripts"));
  for (const file of ["go-vuln-json.go", "go-vuln-lxd.json"])
    copyFileSync(
      path.join(repository, "scripts", file),
      path.join(root, "scripts", file),
    );
  writeFileSync(path.join(root, ".gitignore"), ".cache/\nrunner/ignored.go\n");
  writeFileSync(
    path.join(root, "runner/go.mod"),
    `module example.com/scanner-fixture\n\ngo ${vulnTool.goVersion}\n`,
  );
  writeFileSync(path.join(root, "runner/go.sum"), "");
  writeFileSync(
    path.join(root, "runner/main.go"),
    "package main\nfunc main() {}\n",
  );
  writeFileSync(
    path.join(root, "runner/coverage_test.go"),
    '//go:build integration\n\npackage main\nimport ("fmt"; "testing")\nfunc TestAnalysis(t *testing.T) { fmt.Println("never execute fixture") }\n',
  );
  const cache = path.join(root, vulnTool.cache);
  mkdirSync(cache, { recursive: true });
  copyFileSync(tool, path.join(cache, "govulncheck"));
  for (const name of ["db", "empty-db"]) {
    mkdirSync(path.join(root, name, "index"), { recursive: true });
    writeFileSync(
      path.join(root, name, "index/db.json"),
      JSON.stringify({ modified: synthetic.modified }),
    );
    writeFileSync(
      path.join(root, name, "index/modules.json"),
      JSON.stringify(
        name === "db"
          ? [
              {
                path: "stdlib",
                vulns: [
                  {
                    id: synthetic.id,
                    modified: synthetic.modified,
                    fixed: "99.0.0",
                  },
                ],
              },
            ]
          : [],
      ),
    );
  }
  mkdirSync(path.join(root, "db/ID"));
  writeFileSync(
    path.join(root, `db/ID/${synthetic.id}.json`),
    JSON.stringify(synthetic),
  );
  return {
    root,
    cache,
    database: `file://${root}/db`,
    cleanDatabase: `file://${root}/empty-db`,
  };
}

async function rejectsOperation(work: Promise<unknown>, pattern: RegExp) {
  await assert.rejects(work, (error) => {
    assert.match(describeFailure(error), pattern);
    return true;
  });
}

test("real scanner detects selected test/tag symbols, keeps reports and leaves locks unchanged", async (t) => {
  const f = fixture(t);
  const before = readFileSync(path.join(f.root, "runner/go.mod"));
  const clean = await checkVulnerabilities({
    ...f,
    database: f.cleanDatabase,
    goBin,
  });
  assert.equal(clean.findings, false);
  assert.equal(clean.blocked, false);
  const hit = await checkVulnerabilities({ ...f, goBin });
  assert.equal(hit.findings, true);
  assert.equal(hit.blocked, true);
  const assessment = JSON.parse(
    readFileSync(path.join(hit.report, "assessment.json"), "utf8"),
  );
  assert.equal(assessment.nativeJSONExitCode, 0);
  assert.equal(assessment.nativeTextExitCode, 3);
  assert.match(
    readFileSync(path.join(hit.report, "04-govulncheck.log"), "utf8"),
    /GO-2099-0001/,
  );
  assert.deepEqual(readFileSync(path.join(f.root, "runner/go.mod")), before);
  assert.equal(existsSync(path.join(hit.report, "source")), false);
  assert.equal(existsSync(path.join(hit.report, "control")), false);
  const control = path.join(f.root, "native");
  mkdirSync(path.join(control, "tmp"), { recursive: true });
  mkdirSync(path.join(control, "config/go/telemetry"), { recursive: true });
  writeFileSync(
    path.join(control, "config/go/telemetry/mode"),
    "off 2026-10-29",
  );
  const env = vulnerabilityEnvironment(go, f.cache, control, false);
  for (const [flags, code] of [
    [["-test", "-tags=integration", "-json"], 0],
    [["-test", "-tags=integration", "-format=sarif"], 0],
    [["-test"], 0],
    [["-tags=integration"], 0],
  ] as const) {
    const result = spawnSync(tool, [`-db=${f.database}`, ...flags, "./..."], {
      cwd: path.join(f.root, "runner"),
      env,
      encoding: "utf8",
      timeout: 60_000,
    });
    assert.equal(result.status, code, result.stderr);
    if (flags.length === 3) assert.match(result.stdout, /GO-2099-0001/);
    else assert.match(result.stdout, /No vulnerabilities found/);
  }
  const conversionFailure = spawnSync(
    tool,
    [`-db=${f.database}`, "-mode=convert", "-format=text"],
    {
      cwd: path.join(f.root, "runner"),
      env,
      input: '{"config":',
      encoding: "utf8",
      timeout: 60_000,
    },
  );
  assert.equal(conversionFailure.status, 1);
  assert.match(conversionFailure.stderr, /EOF/);
});

test("standard library decoder rejects malformed event streams", () => {
  const result = spawnSync(
    go,
    [
      "test",
      "-count=1",
      path.join(repository, "scripts/go-vuln-json.go"),
      path.join(repository, "scripts/go-vuln-json_test.go"),
    ],
    {
      cwd: repository,
      env: {
        PATH: `${goBin}:/usr/bin:/bin`,
        GOENV: "off",
        GOWORK: "off",
        GOTOOLCHAIN: "local",
        GOPROXY: "off",
        GOSUMDB: "off",
        GOCACHE: path.join(repository, ".cache/designs-go-vuln/build"),
      },
      encoding: "utf8",
      timeout: 60_000,
    },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

function dispositionFixture() {
  const record = JSON.parse(
    readFileSync(path.join(repository, "scripts/go-vuln-lxd.json"), "utf8"),
  );
  const osvs = lxdIDs.map((id) => ({
    id,
    modified: "2026-10-29T00:00:00Z",
    affected: [],
  }));
  const config = {
    protocol_version: "v1.0.0",
    scanner_name: "govulncheck",
    scanner_version: vulnTool.version,
    go_version: `go${vulnTool.goVersion}`,
    scan_level: "symbol",
    scan_mode: "source",
    db: vulnTool.database,
  };
  const sbom = {
    go_version: `go${vulnTool.goVersion}`,
    roots: ["example.com/runner"],
    modules: [{ path: lxdModule, version: record.module.version }],
  };
  const messages: Record<string, unknown>[] = [
    { config },
    { SBOM: sbom },
    ...osvs.map((osv) => ({ osv })),
    ...lxdIDs.map((osv) => ({
      finding: {
        osv,
        trace: [
          {
            module: lxdModule,
            version: record.module.version,
            package: lxdPackages[0],
            function: "ConnectLXDUnixWithContext",
          },
        ],
      },
    })),
  ];
  for (const a of record.advisories)
    a.sha256 = hashJSON(osvs.find((o) => o.id === a.id));
  record.scope = scopeIdentity(vulnTool);
  const actual = {
    identity: { ...vulnTool } as ScanIdentity,
    sourceSHA256: record.sourceSHA256 as string,
    goSum: `${lxdModule} ${record.module.version} ${record.module.sum}\n${lxdModule} ${record.module.version}/go.mod ${record.module.goModSum}\n`,
    graph: {
      roots: ["example.com/runner"],
      lxd: [...lxdPackages],
      unsupported: [] as string[],
    },
  };
  return { record, messages, actual };
}

test("only the five bound LXD findings can be disposed, while native findings remain true", () => {
  const f = dispositionFixture(),
    report = parseScan(f.messages, vulnTool);
  const result = evaluateLXD(report, f.record, f.actual);
  assert.deepEqual(result.disposed, lxdIDs);
  assert.deepEqual(result.pending, []);
  assert.equal(verifyNativeStatus(report, 3), true);
  assert.throws(() => verifyNativeStatus(report, 0), /disagrees/);
  f.messages.push(
    {
      osv: {
        id: "GO-2099-0002",
        modified: "2026-10-29T00:00:00Z",
        affected: [],
      },
    },
    {
      finding: {
        osv: "GO-2099-0002",
        trace: [
          {
            module: lxdModule,
            version: f.record.module.version,
            package: lxdPackages[0],
            function: "NewCall",
          },
        ],
      },
    },
  );
  assert.deepEqual(
    evaluateLXD(parseScan(f.messages, vulnTool), f.record, f.actual).pending,
    ["GO-2099-0002"],
  );
  (
    f.messages.at(-1)!.finding as { trace: Record<string, unknown>[] }
  ).trace[0]!.function = undefined;
  assert.deepEqual(
    evaluateLXD(parseScan(f.messages, vulnTool), f.record, f.actual).pending,
    [],
  );
});

test("source, locks, graph, scope and OSV changes invalidate the reviewed set", () => {
  const changes: ((f: ReturnType<typeof dispositionFixture>) => void)[] = [
    (f) => {
      f.actual.sourceSHA256 = "f".repeat(64);
    },
    (f) => {
      f.actual.goSum = "";
    },
    (f) => {
      f.actual.graph.lxd.pop();
    },
    (f) => {
      f.actual.graph.lxd.push(`${lxdModule}/lxd`);
    },
    (f) => {
      f.actual.graph.unsupported.push("runner: EmbedFiles");
    },
    (f) => {
      f.actual.identity.hash = "f".repeat(64);
    },
    (f) => {
      f.record.scope.tags = [];
    },
    (f) => {
      (f.messages[2]!.osv as Record<string, unknown>).modified =
        "2026-10-30T00:00:00Z";
    },
    (f) => {
      (
        f.messages[1]!.SBOM as { modules: Record<string, unknown>[] }
      ).modules[0]!.version = "v9.0.0";
    },
  ];
  for (const change of changes) {
    const f = dispositionFixture();
    change(f);
    const result = evaluateLXD(
      parseScan(f.messages, vulnTool),
      f.record,
      f.actual,
    );
    assert.deepEqual(result.disposed, []);
    assert.deepEqual(result.pending, lxdIDs);
    assert.ok(result.reasons.length);
  }
  for (const field of ["module", "version", "package"]) {
    const f = dispositionFixture();
    (
      f.messages.at(-1)!.finding as { trace: Record<string, unknown>[] }
    ).trace[0]![field] = "other";
    assert.deepEqual(
      evaluateLXD(parseScan(f.messages, vulnTool), f.record, f.actual).pending,
      [lxdIDs.at(-1)],
    );
  }
});

test("source binding includes added, deleted and changed Go files and locks, but excludes Markdown", () => {
  const entries = [
    { path: "runner/main.go", sha256: "a" },
    { path: "runner/main_test.go", sha256: "b" },
    { path: "runner/go.mod", sha256: "c" },
    { path: "runner/go.sum", sha256: "d" },
  ];
  const expected = runnerInputHash(entries);
  assert.equal(
    runnerInputHash([
      ...entries,
      { path: "runner/README.md", sha256: "changed" },
    ]),
    expected,
  );
  assert.notEqual(runnerInputHash(entries.slice(1)), expected);
  assert.notEqual(
    runnerInputHash([...entries, { path: "runner/new.go", sha256: "e" }]),
    expected,
  );
  for (let i = 0; i < entries.length; i++)
    assert.notEqual(
      runnerInputHash(
        entries.map((e, j) => (j === i ? { ...e, sha256: "changed" } : e)),
      ),
      expected,
    );
});

test("go list metadata detects non-Go and embedded main-module inputs without rejecting dependency inputs", () => {
  for (const field of [
    "CgoFiles",
    "CFiles",
    "CXXFiles",
    "MFiles",
    "HFiles",
    "FFiles",
    "SFiles",
    "SwigFiles",
    "SwigCXXFiles",
    "SysoFiles",
    "IgnoredOtherFiles",
    "EmbedFiles",
    "EmbedPatterns",
    "TestEmbedFiles",
    "TestEmbedPatterns",
    "XTestEmbedFiles",
    "XTestEmbedPatterns",
  ]) {
    const p = {
      ImportPath: "example.com/runner",
      Module: { Path: "example.com/runner" },
      [field]: ["input"],
    };
    assert.equal(
      inspectPackages([p], "example.com/runner").unsupported.length,
      1,
    );
    assert.equal(inspectPackages([p], "other").unsupported.length, 0);
  }
  assert.throws(
    () => inspectPackages([{ ImportPath: "x", Incomplete: true }], "x"),
    /Incomplete/,
  );
});

test("report identity, associations and disposition shape fail closed", () => {
  assert.throws(() => parseScan([], vulnTool), /Incomplete/);
  const repeated = dispositionFixture();
  repeated.messages.push(structuredClone(repeated.messages[2]!));
  assert.equal(parseScan(repeated.messages, vulnTool).osvs.size, 5);
  (repeated.messages.at(-1)!.osv as Record<string, unknown>).modified =
    "changed";
  assert.throws(
    () => parseScan(repeated.messages, vulnTool),
    /Conflicting OSV/,
  );
  const mutations: ((f: ReturnType<typeof dispositionFixture>) => void)[] = [
    (f) => {
      f.messages.splice(1, 1);
    },
    (f) => {
      f.messages.push(f.messages[0]!);
    },
    (f) => {
      (f.messages[0]!.config as Record<string, unknown>).protocol_version =
        "v2";
    },
    (f) => {
      (f.messages[0]!.config as Record<string, unknown>).scan_mode = "binary";
    },
    (f) => {
      f.messages.push({ mystery: {} });
    },
    (f) => {
      f.messages.splice(2, 1);
    },
    (f) => {
      (f.messages.at(-1)!.finding as { trace: unknown[] }).trace = [];
    },
  ];
  for (const mutate of mutations) {
    const f = dispositionFixture();
    mutate(f);
    assert.throws(() => parseScan(f.messages, vulnTool));
  }
  for (const mutate of [
    (r: Record<string, unknown>) => {
      r.schema = 2;
    },
    (r: Record<string, unknown>) => {
      delete r.sourceSHA256;
    },
    (r: Record<string, unknown>) => {
      r.packages = [];
    },
    (r: Record<string, unknown>) => {
      r.advisories = [];
    },
    (r: Record<string, unknown>) => {
      r.ignore = "*";
    },
  ]) {
    const f = dispositionFixture();
    mutate(f.record);
    assert.throws(() =>
      evaluateLXD(parseScan(f.messages, vulnTool), f.record, f.actual),
    );
  }
});

test("database and missing or forged tool failures never become vulnerability results", async (t) => {
  const f = fixture(t);
  await rejectsOperation(
    checkVulnerabilities({ ...f, database: `file://${f.root}/absent`, goBin }),
    /no such file|ENOENT/,
  );
  const binary = path.join(f.cache, "govulncheck");
  rmSync(binary);
  await rejectsOperation(checkVulnerabilities({ ...f, goBin }), /ENOENT/);
  writeFileSync(
    binary,
    `#!/bin/sh\necho 'Scanner: govulncheck@${vulnTool.version}'\n`,
    { mode: 0o700 },
  );
  await rejectsOperation(checkVulnerabilities({ ...f, goBin }), /checksum/);
});

test("source selection excludes ignored additions and refuses symlinks and missing locks", async (t) => {
  const f = fixture(t);
  writeFileSync(
    path.join(f.root, "runner/ignored.go"),
    "not valid Go source\n",
  );
  assert.equal(
    (await checkVulnerabilities({ ...f, database: f.cleanDatabase, goBin }))
      .findings,
    false,
  );
  symlinkSync("/etc/passwd", path.join(f.root, "runner/linked.go"));
  await rejectsOperation(checkVulnerabilities({ ...f, goBin }), /regular file/);
  rmSync(path.join(f.root, "runner/linked.go"));
  rmSync(path.join(f.root, "runner/go.sum"));
  await rejectsOperation(checkVulnerabilities({ ...f, goBin }), /module locks/);
});

test("ambient Go, proxies, tokens and fixture switches do not enter the scanner environment", () => {
  const names = [
    "GOPROXY",
    "GOVULNDB",
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "GITHUB_TOKEN",
    "GUARD_CREATE_HELPER",
    "WATCHDOG_STOP_DIR",
    "RUNNER_TEST_MODE",
  ];
  const previous = names.map((name) => process.env[name]);
  try {
    for (const name of names) process.env[name] = "poison";
    const env = vulnerabilityEnvironment(go, "/cache", "/control", false);
    for (const name of names) assert.notEqual(env[name], "poison");
    assert.equal(env.GOPROXY, "off");
    assert.equal(env.GOENV, "off");
    assert.equal(env.GOFLAGS, "-mod=readonly");
  } finally {
    names.forEach((name, i) => {
      if (previous[i] === undefined) delete process.env[name];
      else process.env[name] = previous[i];
    });
  }
});

async function databaseServer(
  t: TestContext,
  root: string,
  action: () => void,
) {
  let fired = false;
  const server = createServer((request, response) => {
    if (!fired && request.url === "/index/modules.json.gz") {
      fired = true;
      action();
    }
    const relative = request.url?.replace(/\.gz$/, "");
    if (
      ![
        "/index/db.json",
        "/index/modules.json",
        `/ID/${synthetic.id}.json`,
      ].includes(relative ?? "")
    ) {
      response.writeHead(404).end();
      return;
    }
    const bytes = readFileSync(path.join(root, "db", relative!));
    response.end(gzipSync(bytes));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => {
    server.closeAllConnections();
    server.close();
  });
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return `http://127.0.0.1:${address.port}`;
}

test("concurrent live source changes fail after an otherwise completed scan", async (t) => {
  const f = fixture(t);
  const database = await databaseServer(t, f.root, () =>
    writeFileSync(
      path.join(f.root, "runner/main.go"),
      "package main\nfunc main() { println(1) }\n",
    ),
  );
  await rejectsOperation(
    checkVulnerabilities({ ...f, database, goBin }),
    /source changed/,
  );
});

test("concurrent disposition changes cannot approve a completed scan", async (t) => {
  const f = fixture(t);
  const database = await databaseServer(t, f.root, () => {
    const file = path.join(f.root, "scripts/go-vuln-lxd.json");
    writeFileSync(file, readFileSync(file, "utf8") + "\n");
  });
  await rejectsOperation(
    checkVulnerabilities({ ...f, database, goBin }),
    /source changed/,
  );
});

test("cancellation during database access stays an operation failure and cleans owned snapshots", async (t) => {
  const f = fixture(t);
  const controller = new AbortController();
  const database = await databaseServer(t, f.root, () =>
    controller.abort(new Error("acceptance cancellation")),
  );
  await rejectsOperation(
    checkVulnerabilities({ ...f, database, goBin, signal: controller.signal }),
    /cancel|interrupt/,
  );
  for (const run of readdirSync(f.cache).filter((name) =>
    name.startsWith("run-"),
  ))
    assert.equal(existsSync(path.join(f.cache, run, "source")), false);
});

test("failed preparation preserves the previous verified installation", async (t) => {
  const f = fixture(t);
  const before = readFileSync(path.join(f.cache, "govulncheck"));
  const fake = path.join(f.root, "sdk");
  mkdirSync(fake);
  writeFileSync(
    path.join(fake, "go"),
    `#!${process.execPath}\nif (process.argv[2] === "version") console.log("go version go${vulnTool.goVersion} linux/amd64"); else if (process.argv[2] === "mod" && process.argv[3] === "edit") require('node:child_process').execFileSync(${JSON.stringify(go)}, process.argv.slice(2), {stdio:'inherit'}); else process.exit(9);\n`,
    { mode: 0o700 },
  );
  await rejectsOperation(
    checkVulnerabilities({ ...f, prepare: true, goBin: fake }),
    /exited 9/,
  );
  assert.deepEqual(readFileSync(path.join(f.cache, "govulncheck")), before);
});

test("command status distinguishes a real exit 3 from cancellation that exits 3", async (t) => {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-vuln-exit-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const options = {
    cwd: root,
    env: {},
    log: path.join(root, "exit.log"),
    timeoutMs: 5_000,
  };
  await assert.rejects(
    runCommand(process.execPath, ["-e", "process.exit(3)"], options),
    (error) =>
      error instanceof CommandFailure &&
      error.exitCode === 3 &&
      error.groupClosed,
  );
  const controller = new AbortController();
  const marker = path.join(root, "ready");
  const work = runCommand(
    process.execPath,
    [
      "-e",
      `process.on('SIGTERM',()=>process.exit(3));require('node:fs').writeFileSync(${JSON.stringify(marker)},'');setInterval(()=>{},1000)`,
    ],
    {
      ...options,
      log: path.join(root, "cancel.log"),
      signal: controller.signal,
    },
  );
  const rejected = assert.rejects(
    work,
    (error) =>
      error instanceof CommandFailure &&
      error.exitCode === undefined &&
      error.groupClosed,
  );
  const deadline = Date.now() + 4_000;
  while (!existsSync(marker) && Date.now() < deadline)
    await new Promise((resolve) => setTimeout(resolve, 10));
  assert.ok(existsSync(marker));
  controller.abort();
  await rejected;
  await assert.rejects(
    runCommand(
      process.execPath,
      [
        "-e",
        "process.on('SIGTERM',()=>process.exit(3));setInterval(()=>{},1000)",
      ],
      { ...options, log: path.join(root, "timeout.log"), timeoutMs: 500 },
    ),
    (error) =>
      error instanceof CommandFailure &&
      error.exitCode === undefined &&
      error.groupClosed,
  );
});

test("absolute module replacement cannot escape the isolated snapshot", async (t) => {
  const f = fixture(t);
  writeFileSync(
    path.join(f.root, "runner/go.mod"),
    `module example.com/scanner-fixture\n\ngo ${vulnTool.goVersion}\nreplace example.com/external => /tmp/external\n`,
  );
  await rejectsOperation(
    checkVulnerabilities({ ...f, goBin }),
    /replacements are unsupported/,
  );
});

test("cleanup failure rejects a completed scan and retains diagnostics", async (t) => {
  const f = fixture(t);
  let blocked: string | undefined;
  const database = await databaseServer(t, f.root, () => {
    const run = readdirSync(f.cache).find((name) => name.startsWith("run-"));
    assert.ok(run);
    blocked = path.join(f.cache, run, "control/cleanup-blocked");
    mkdirSync(blocked);
    writeFileSync(path.join(blocked, "owned"), "cleanup acceptance");
    chmodSync(blocked, 0);
  });
  try {
    await rejectsOperation(
      checkVulnerabilities({ ...f, database, goBin }),
      /EACCES|permission denied/,
    );
    assert.ok(blocked);
    const report = path.dirname(path.dirname(blocked));
    assert.ok(existsSync(path.join(report, "04-govulncheck.log")));
    assert.ok(
      existsSync(path.join(report, "assessment.json")),
      "scan and native conversion completed before cleanup failure",
    );
  } finally {
    if (blocked) chmodSync(blocked, 0o700);
  }
});

test("snapshot lock changes remain an operation failure alongside the command error", async (t) => {
  const f = fixture(t);
  const fake = path.join(f.root, "sdk");
  mkdirSync(fake);
  writeFileSync(
    path.join(fake, "go"),
    `#!${process.execPath}\nif(process.argv[2]==='version') console.log('go version go${vulnTool.goVersion} linux/amd64'); else {const fs=require('node:fs');fs.chmodSync('go.sum',0o600);fs.writeFileSync('go.sum','changed');process.exit(9);}\n`,
    { mode: 0o700 },
  );
  await assert.rejects(checkVulnerabilities({ ...f, goBin: fake }), (error) => {
    const text = describeFailure(error);
    assert.match(text, /exited 9/);
    assert.match(text, /changed source or module locks/);
    return true;
  });
  assert.equal(readFileSync(path.join(f.root, "runner/go.sum"), "utf8"), "");
});
