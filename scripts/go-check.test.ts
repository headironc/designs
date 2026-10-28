import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  accessSync,
  constants,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import test, { type TestContext } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import {
  CommandFailure,
  parseMode,
  runCommand,
  verifyTestEvents,
} from "./go-check.ts";

const repository = path.resolve(import.meta.dirname, "..");
const cli = path.join(import.meta.dirname, "go-check.ts");
const lifecycle = "github.com/headironc/designs/runner/internal/lifecycle";
const processTest = "TestActualProcessIdentityAndExit";
const privateTest = "TestDatabaseChildInPrivateContainer";
const rootInstalled = [
  "TestInstalledHistoricalMatchAndReadOnlyReentry",
  ...[
    "content",
    "size",
    "mode",
    "special_mode",
    "gid",
    "uid",
    "missing",
    "symlink",
    "hardlink",
    "fifo",
    "parent",
    "xattr",
  ].map((name) => `TestInstalledFileStates/${name}`),
  ...[
    "lock_missing",
    "lock_busy",
    "lock_link",
    "lock_mode",
    "plan_missing",
    "plan_digest",
    "plan_null",
    "plan_extra",
    "plan_xattrs",
    "producer",
  ].map((name) => `TestInstalledInputsBlockFileReads/${name}`),
  ...[
    "replace",
    "same_size",
    "appear",
    "xattr",
    "parent_replace",
    "plan_change",
    "lock_replace",
    "cancel",
    "fifo_open_race",
  ].map((name) => `TestInstalledConcurrentChangesAndCancellation/${name}`),
];
const ordinaryInstalled = [
  "TestInstalledFileStates",
  "TestInstalledInputsBlockFileReads",
  "TestInstalledConcurrentChangesAndCancellation",
  "TestInstalledStreamingBoundsAndPrivateFiles",
  "TestInstalledCLIAndOutputBoundary",
];

function installedEvents(
  privateContainer: boolean,
): { Action: string; Package: string; Test: string }[] {
  return [...rootInstalled, ...ordinaryInstalled].flatMap((name) => [
    { Action: "run", Package: lifecycle, Test: name },
    {
      Action:
        !privateContainer && rootInstalled.includes(name) ? "skip" : "pass",
      Package: lifecycle,
      Test: name,
    },
  ]);
}

function containerOutput(race: boolean): string {
  return (
    [...(race ? [] : [privateTest]), ...rootInstalled, ...ordinaryInstalled]
      .map((name) => `=== RUN   ${name}\n--- PASS: ${name} (0.00s)\n`)
      .join("") + "PASS\n"
  );
}

function fixture(t: TestContext): string {
  mkdirSync(path.join(repository, ".cache"), { recursive: true });
  const root = mkdtempSync(path.join(repository, ".cache/go-tools-test-"));
  writeFileSync(path.join(root, "package.json"), '{"type":"commonjs"}\n');
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return root;
}

function events(processAction = "pass", extraSkip = false): string {
  const values = [
    { Action: "run", Package: lifecycle, Test: processTest },
    { Action: processAction, Package: lifecycle, Test: processTest },
    { Action: "run", Package: lifecycle, Test: privateTest },
    { Action: "skip", Package: lifecycle, Test: privateTest },
    ...installedEvents(false),
    ...(extraSkip
      ? [{ Action: "skip", Package: lifecycle, Test: "NewUncoveredTest" }]
      : []),
    { Action: "pass", Package: lifecycle },
  ];
  return values.map((value) => JSON.stringify(value)).join("\n") + "\n";
}

test("full checks are the default and unknown or empty stage selections fail", () => {
  assert.deepEqual(parseMode([]), ["module", "container", "nft"]);
  assert.equal(parseMode(["--prepare"]), "prepare");
  assert.deepEqual(parseMode(["--stage=nft"]), ["nft"]);
  for (const args of [
    ["--stage="],
    ["--stage=other"],
    ["--stage=module", "--stage=nft"],
    ["--prepare", "--stage=module"],
  ])
    assert.throws(() => parseMode(args));
});

test("Go events require actual process execution and reject unknown skips, absence and duplication", () => {
  verifyTestEvents(events(), "module");
  assert.throws(
    () => verifyTestEvents(events("skip"), "module"),
    /Unexpected skipped/,
  );
  assert.throws(
    () => verifyTestEvents(events("pass", true), "module"),
    /Unexpected skipped/,
  );
  assert.throws(
    () =>
      verifyTestEvents(
        events()
          .split("\n")
          .filter((line) => !line.includes(processTest))
          .join("\n"),
        "module",
      ),
    /Required Go test/,
  );
  assert.throws(
    () => verifyTestEvents(events() + events(), "module"),
    /exactly once/,
  );
  assert.throws(() => verifyTestEvents("", "module"), /No package passed/);
  assert.throws(() => verifyTestEvents("invalid\n", "module"));
  const packageSkip =
    JSON.stringify({ Action: "skip", Package: "known-command" }) + "\n";
  verifyTestEvents(events() + packageSkip, "module", ["known-command"]);
  assert.throws(
    () => verifyTestEvents(events() + packageSkip, "module"),
    /Unexpected skipped package/,
  );
  for (const stage of ["container", "nft"] as const)
    assert.throws(() => verifyTestEvents(events(), stage));
});

test("installed fixtures require exact module skips and complete normal/race passes", () => {
  const serialized = (items: unknown[]) =>
    items.map((item) => JSON.stringify(item)).join("\n") + "\n";
  const installed = installedEvents(true);
  const passed = { Action: "pass", Package: lifecycle };
  const database = [
    { Action: "run", Package: lifecycle, Test: privateTest },
    { Action: "pass", Package: lifecycle, Test: privateTest },
  ];
  verifyTestEvents(
    serialized([...database, ...installed, passed]),
    "container",
  );
  verifyTestEvents(serialized([...installed, passed]), "container-race");
  assert.throws(
    () => verifyTestEvents(serialized([...installed, passed]), "container"),
    /Required Go test/,
  );
  assert.throws(
    () =>
      verifyTestEvents(
        events() +
          serialized([
            {
              Action: "skip",
              Package: lifecycle,
              Test: "TestInstalledFileStates/new_case",
            },
          ]),
        "module",
      ),
    /Unexpected skipped test/,
  );
  assert.throws(
    () =>
      verifyTestEvents(
        events().replace(
          '"Action":"pass","Package":"' +
            lifecycle +
            '","Test":"TestInstalledStreamingBoundsAndPrivateFiles"',
          '"Action":"skip","Package":"' +
            lifecycle +
            '","Test":"TestInstalledStreamingBoundsAndPrivateFiles"',
        ),
        "module",
      ),
    /Unexpected skipped test/,
  );
  for (const stage of ["container", "container-race"] as const) {
    const input = [
      ...(stage === "container" ? database : []),
      ...installed,
      passed,
    ];
    assert.throws(
      () =>
        verifyTestEvents(
          serialized(
            input.filter(
              (event) =>
                !("Test" in event) ||
                event.Test !== "TestInstalledFileStates/xattr",
            ),
          ),
          stage,
        ),
      /Required Go test/,
    );
    assert.throws(
      () =>
        verifyTestEvents(
          serialized([
            ...input,
            {
              Action: "skip",
              Package: lifecycle,
              Test: "TestInstalledFileStates/content",
            },
          ]),
          stage,
        ),
      /Unexpected skipped test/,
    );
    assert.throws(
      () =>
        verifyTestEvents(
          serialized([
            ...input,
            {
              Action: "pass",
              Package: lifecycle,
              Test: "TestInstalledCLIAndOutputBoundary",
            },
          ]),
          stage,
        ),
      /exactly once/,
    );
  }
});

test("command timeout escalates and cancellation reaps the command's real child", async (t) => {
  const root = fixture(t);
  const originalMask = process.umask();
  assert.equal(
    (
      await runCommand(
        process.execPath,
        ["-e", "console.log(process.umask().toString(8))"],
        {
          cwd: root,
          env: process.env,
          log: path.join(root, "umask.log"),
          timeoutMs: 5_000,
        },
      )
    ).trim(),
    "22",
  );
  assert.equal(process.umask(), originalMask);
  await assert.rejects(
    runCommand(path.join(root, "missing-command"), [], {
      cwd: root,
      env: process.env,
      log: path.join(root, "missing.log"),
      timeoutMs: 5_000,
    }),
    /ENOENT/,
  );
  assert.equal(process.umask(), originalMask);
  await assert.rejects(
    runCommand(
      process.execPath,
      ["-e", "process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)"],
      {
        cwd: root,
        env: process.env,
        log: path.join(root, "timeout.log"),
        timeoutMs: 200,
      },
    ),
    /timed out/,
  );
  const marker = path.join(root, "child.json");
  const script = `
    const {spawn} = require("node:child_process");
    const fs = require("node:fs");
    const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], {stdio:"ignore"});
    process.on("SIGTERM", () => {});
    child.on("exit", () => process.exit(0));
    fs.writeFileSync(${JSON.stringify(marker)}, JSON.stringify({pid:process.pid, child:child.pid}));
  `;
  const controller = new AbortController();
  const pending = runCommand(process.execPath, ["-e", script], {
    cwd: root,
    env: process.env,
    log: path.join(root, "cancel.log"),
    timeoutMs: 10_000,
    signal: controller.signal,
  });
  const rejected = assert.rejects(pending, /cancelled/);
  let pids: { pid: number; child: number } | undefined;
  try {
    const deadline = Date.now() + 5_000;
    while (!pids && Date.now() < deadline) {
      try {
        pids = JSON.parse(readFileSync(marker, "utf8"));
      } catch {
        await delay(10);
      }
    }
    assert.ok(pids, "real child must start before cancellation");
    controller.abort();
    await rejected;
    for (const pid of [pids.pid, pids.child])
      assert.throws(
        () => process.kill(pid, 0),
        (error: unknown) => (error as NodeJS.ErrnoException).code === "ESRCH",
      );
  } finally {
    controller.abort();
    await rejected;
  }
});

test("module supervision reaps separate groups and sessions without affecting another process", async (t) => {
  const root = fixture(t);
  const supervisor = path.join(root, "go-supervise");
  const target = path.join(root, "descendants");
  const source = path.join(root, "descendants.go");
  writeFileSync(
    source,
    `package main
import("encoding/json"; "os"; "os/exec"; "os/signal"; "strings"; "syscall"; "time")
func main() {
  mode, marker := os.Args[1], os.Args[2]
  if mode == "leaf" {
    signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
    raw, _ := os.ReadFile("/proc/self/stat")
    fields := strings.Fields(string(raw)[strings.LastIndex(string(raw), ")")+2:])
    value, _ := json.Marshal(map[string]any{"pid":os.Getpid(),"start":fields[19]})
    if err := os.WriteFile(marker, value, 0600); err != nil { panic(err) }
  } else {
    child := exec.Command(os.Args[0], "leaf", marker)
    child.SysProcAttr = &syscall.SysProcAttr{Setpgid: !strings.HasSuffix(mode,"session"), Setsid: strings.HasSuffix(mode,"session")}
    if err := child.Start(); err != nil { panic(err) }
    limit := time.Now().Add(5*time.Second)
    for { if _, err := os.Stat(marker); err == nil { break }; if time.Now().After(limit) { panic("leaf did not start") }; time.Sleep(time.Millisecond) }
    if strings.HasPrefix(mode,"orphan") { return }
    signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
  }
  for { time.Sleep(time.Second) }
}
`,
  );
  const buildEnv = {
    ...process.env,
    GOTOOLCHAIN: "local",
    GOWORK: "off",
    GOENV: "off",
    CGO_ENABLED: "0",
    GOCACHE: path.join(repository, "runner/.build/go-cache"),
    GOMODCACHE: path.join(repository, "runner/.build/go-mod"),
  };
  execFileSync(
    "go",
    [
      "build",
      "-o",
      supervisor,
      path.join(repository, "scripts/go-supervise.go"),
    ],
    { env: buildEnv, timeout: 120000 },
  );
  execFileSync("go", ["build", "-o", target, source], {
    env: buildEnv,
    timeout: 120000,
  });
  const unrelated = spawn(
    process.execPath,
    ["-e", "setInterval(() => {}, 1000)"],
    { stdio: "ignore" },
  );
  const unrelatedClosed = new Promise<void>((resolve) =>
    unrelated.once("close", () => resolve()),
  );
  t.after(async () => {
    unrelated.kill("SIGKILL");
    await unrelatedClosed;
  });
  let sequence = 0;
  const options = () => ({
    cwd: root,
    env: process.env,
    log: path.join(root, `${++sequence}.log`),
    timeoutMs: 10000,
    supervisor,
  });
  const completeFailure = (error: unknown) => {
    assert.ok(error instanceof CommandFailure);
    assert.equal(error.groupClosed, true, error.message);
    return true;
  };
  await t.test(
    "normal success, target failure and target start failure retain separate cleanup facts",
    async () => {
      assert.equal(
        (
          await runCommand(
            process.execPath,
            ["-e", "console.log('success')"],
            options(),
          )
        ).trim(),
        "success",
      );
      await assert.rejects(
        runCommand(process.execPath, ["-e", "process.exit(7)"], options()),
        (error: unknown) => {
          completeFailure(error);
          assert.match((error as Error).message, /target exited 7/);
          return true;
        },
      );
      await assert.rejects(
        runCommand(path.join(root, "missing-command"), [], options()),
        (error: unknown) => {
          completeFailure(error);
          assert.equal((error as CommandFailure).started, false);
          return true;
        },
      );
    },
  );
  type Identity = { pid: number; start: string };
  const identities: Identity[] = [];
  const alive = (identity: Identity) => {
    try {
      const raw = readFileSync(`/proc/${identity.pid}/stat`, "utf8");
      return (
        raw.slice(raw.lastIndexOf(")") + 2).split(" ")[19] === identity.start
      );
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
      throw error;
    }
  };
  t.after(() => {
    for (const identity of identities)
      if (alive(identity)) process.kill(identity.pid, "SIGKILL");
  });
  for (const mode of [
    "orphan-group",
    "orphan-session",
    "cancel-group",
    "timeout-session",
  ] as const) {
    await t.test(mode, async () => {
      const marker = path.join(root, `${mode}.json`);
      const controller = new AbortController();
      const pending = runCommand(target, [mode, marker], {
        ...options(),
        signal: controller.signal,
        timeoutMs: mode === "timeout-session" ? 500 : 10000,
      });
      const rejected = assert.rejects(pending, (error: unknown) => {
        completeFailure(error);
        assert.match(
          (error as Error).message,
          mode.startsWith("orphan")
            ? /surviving descendants/
            : mode.startsWith("cancel")
              ? /cancelled/
              : /timed out/,
        );
        return true;
      });
      const deadline = Date.now() + 5000;
      while (!existsSync(marker) && Date.now() < deadline) await delay(10);
      assert.ok(
        existsSync(marker),
        `${mode} did not create its real descendant`,
      );
      const identity = JSON.parse(readFileSync(marker, "utf8")) as Identity;
      identities.push(identity);
      if (mode.startsWith("cancel")) controller.abort();
      await rejected;
      assert.equal(
        alive(identity),
        false,
        `${mode} left its descendant alive or unreaped`,
      );
      assert.equal(unrelated.exitCode, null);
      assert.equal(unrelated.signalCode, null);
      assert.doesNotThrow(() => process.kill(unrelated.pid!, 0));
    });
  }
  await t.test(
    "missing, malformed and forcibly terminated supervisor receipts remain unknown",
    async () => {
      for (const body of [
        "process.exit(0)",
        "require('node:fs').writeSync(3, '[1,2,0,null,true,[]]\\n[1,2,0,null,true,[]]\\n')",
        "require('node:fs').writeSync(3, '[2,2,0,null,true,[]]\\n')",
        "process.kill(process.pid, 'SIGKILL')",
      ]) {
        const broken = path.join(root, `broken-${sequence}`);
        writeFileSync(broken, `#!${process.execPath}\n${body}\n`, {
          mode: 0o755,
        });
        await assert.rejects(
          runCommand(process.execPath, ["-e", "process.exit(0)"], {
            ...options(),
            supervisor: broken,
          }),
          (error: unknown) => {
            assert.ok(error instanceof CommandFailure);
            assert.equal(error.groupClosed, false);
            assert.match(error.message, /Supervisor cleanup unknown/);
            return true;
          },
        );
      }
    },
  );
});

function fakeGo(
  root: string,
  mode:
    | "failure"
    | "skip"
    | "container"
    | "container-pass"
    | "lock-change"
    | "lock-missing"
    | "lock-change-success"
    | "prepare-lock-change",
): void {
  const script = `#!${process.execPath}
    const fs = require("node:fs");
    const args = process.argv.slice(2);
    if(Object.keys(process.env).some(key => ["RUNNER_", "GUARD_", "WATCHDOG_STOP_"].some(prefix => key.startsWith(prefix))) || process.env.GOROOT || process.env.GOFLAGS !== "-mod=readonly" || process.env.GOPROXY !== (${JSON.stringify(mode)} === "prepare-lock-change" ? "https://proxy.golang.org" : "off")) { console.error("ambient Go or fixture settings leaked"); process.exit(91); }
    if (args[0] === "version") { console.log("go version go1.27.3 linux/amd64"); process.exit(0); }
    if (args[0] === "mod" && args[1] === "download" && ${JSON.stringify(mode)} === "prepare-lock-change") { fs.appendFileSync("go.sum", "changed\\n"); process.exit(0); }
    if (args[0] === "build" && args.at(-1).endsWith("go-supervise.go")) {
      const result = require("node:child_process").spawnSync("go", args, {env:{...process.env,PATH:${JSON.stringify(process.env.PATH)}},stdio:"inherit"});
      process.exit(result.status ?? 1);
    }
    if (args[0] === "test" && args.includes("-c")) {
      if (${JSON.stringify(mode)} === "container-pass" && args.includes("-race")) {
        const source = ${JSON.stringify("#include <stdio.h>\nint main(void) { fputs(" + JSON.stringify(containerOutput(true)) + ", stdout); return 0; }\n")};
        const result = require("node:child_process").spawnSync("gcc", ["-x", "c", "-o", args[args.indexOf("-o")+1], "-"], {input:source,encoding:"utf8"});
        if (result.stderr) process.stderr.write(result.stderr);
        process.exit(result.status ?? 1);
      }
      const body = ${JSON.stringify(mode)} === "container-pass"
        ? ${JSON.stringify("#!/bin/sh\nprintf '%s' '" + containerOutput(false) + "'\n")}
        : "#!/bin/sh\\nprintf 'private container started before cancellation\\n'\\nexec sleep 120\\n";
      fs.writeFileSync(args[args.indexOf("-o")+1], body, {mode:0o755}); process.exit(0);
    }
    if (args[0] === "tool") {
      const result = require("node:child_process").spawnSync("go", args, {env:{...process.env,PATH:${JSON.stringify(process.env.PATH)}},stdio:"inherit"});
      process.exit(result.status ?? 1);
    }
    if (args[0] === "test") {
      if (["lock-change", "lock-change-success"].includes(${JSON.stringify(mode)})) fs.appendFileSync("go.sum", "changed\\n");
      if (${JSON.stringify(mode)} === "lock-missing") fs.unlinkSync("go.sum");
      if (["failure", "lock-change", "lock-missing"].includes(${JSON.stringify(mode)})) { console.error("intentional test command failure"); process.exit(3); }
      if (${JSON.stringify(mode)} === "lock-change-success") { process.stdout.write(${JSON.stringify(events())}); process.exit(0); }
      process.stdout.write(${JSON.stringify(events("pass", true))}); process.exit(0);
    }
    process.exit(0);
  `;
  writeFileSync(path.join(root, "go"), script, { mode: 0o755 });
}

function resolveExecutable(name: string, searchPath: string): string {
  for (const directory of searchPath.split(path.delimiter)) {
    const candidate = path.resolve(directory, name);
    try {
      accessSync(candidate, constants.X_OK);
      if (statSync(candidate).isFile()) return candidate;
    } catch (error) {
      if (
        !["ENOENT", "EACCES", "ENOTDIR"].includes(
          (error as NodeJS.ErrnoException).code ?? "",
        )
      )
        throw error;
    }
  }
  throw new Error(`Executable is unavailable on the original PATH: ${name}`);
}

test("Docker resolution honors an executable outside /usr/bin", (t) => {
  const root = fixture(t);
  const executable = path.join(root, "docker");
  writeFileSync(
    executable,
    `#!${process.execPath}\nconsole.log("private Docker path");\n`,
    { mode: 0o755 },
  );
  const resolved = resolveExecutable(
    "docker",
    `${root}${path.delimiter}/usr/bin`,
  );
  assert.equal(resolved, executable);
  assert.equal(
    execFileSync(resolved, [], { encoding: "utf8" }).trim(),
    "private Docker path",
  );
});

// Independent teardown also runs when an assertion or the CLI's cleanup fails.
function cleanupContainer(name: string, dockerExecutable: string): void {
  const docker = (args: string[]) =>
    execFileSync(dockerExecutable, args, {
      encoding: "utf8",
      timeout: 10_000,
    });
  const lookup = (filter: string) =>
    docker([
      "container",
      "ls",
      "--all",
      "--no-trunc",
      "--filter",
      filter,
      "--format",
      "{{.ID}}",
    ]).trim();
  const id = lookup(`name=^/${name}$`);
  if (!id) return;
  assert.match(id, /^[a-f0-9]{64}$/);
  const state = () =>
    JSON.parse(docker(["inspect", "--format", "{{json .State}}", id])) as {
      Running: boolean;
      Status: string;
    };
  let current = state();
  if (current.Running) {
    try {
      docker(["stop", "--time=2", id]);
    } catch (error) {
      console.error(
        `Test teardown stop requires readback: ${name}: ${String(error)}`,
      );
    }
    current = state();
    if (current.Running) {
      try {
        docker(["kill", id]);
      } catch (error) {
        console.error(
          `Test teardown kill requires readback: ${name}: ${String(error)}`,
        );
      }
      current = state();
    }
  }
  assert.ok(
    !current.Running && ["created", "exited", "dead"].includes(current.Status),
    `Test container is not terminal: ${name}/${id}`,
  );
  try {
    docker(["rm", id]);
  } catch (error) {
    console.error(
      `Test teardown removal requires readback: ${name}: ${String(error)}`,
    );
  }
  assert.equal(lookup(`id=${id}`), "", `Test container ID still exists: ${id}`);
  assert.equal(
    lookup(`name=^/${name}$`),
    "",
    `Test container name still exists: ${name}`,
  );
}

test("the actual CLI fails for a missing tool, test failure and unexpected skip", (t) => {
  for (const mode of ["missing", "failure", "skip"] as const) {
    const root = fixture(t);
    if (mode !== "missing") fakeGo(root, mode);
    const result = spawnSync(process.execPath, [cli, "--stage=module"], {
      cwd: repository,
      env: {
        ...process.env,
        PATH: mode === "missing" ? root : `${root}:${process.env.PATH}`,
        RUNNER_DIAGNOSTIC_PRIVATE_CONTAINER: "1",
        GUARD_CREATE_HELPER: path.join(root, "external-create"),
        GUARD_WRITE_COMMAND: "DeleteInstance",
        GUARD_LOCK_HELPER: path.join(root, "external-lock"),
        GUARD_TX_ROOT: path.join(root, "external-state"),
        GUARD_TX_PHASE: "renamed",
        WATCHDOG_STOP_HELPER: "1",
        WATCHDOG_STOP_DIR: path.join(root, "external-stop"),
        WATCHDOG_STOP_BUS: "unix:path=/unreachable-fixture-bus",
        GOROOT: "/wrong-sdk",
      },
      encoding: "utf8",
      timeout: 15_000,
    });
    assert.ifError(result.error);
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(
      result.stderr,
      mode === "missing"
        ? /ENOENT/
        : mode === "failure"
          ? /exited 3/
          : /Unexpected skipped test/,
    );
    if (mode === "failure")
      assert.match(result.stderr, /intentional test command failure/);
    assert.doesNotMatch(
      result.stdout,
      /Go stage passed|Complete Go check passed/,
    );
  }
});

function isolatedCLI(root: string): string {
  mkdirSync(path.join(root, "scripts"));
  mkdirSync(path.join(root, "runner"));
  const copied = path.join(root, "scripts/go-check.ts");
  writeFileSync(copied, readFileSync(cli));
  writeFileSync(
    path.join(root, "scripts/go-supervise.go"),
    readFileSync(path.join(repository, "scripts/go-supervise.go")),
  );
  writeFileSync(path.join(root, "package.json"), '{"type":"module"}\n');
  writeFileSync(
    path.join(root, "runner/go.mod"),
    "module fixture\n\ngo 1.27.3\n",
  );
  writeFileSync(path.join(root, "runner/go.sum"), "original lock bytes\n");
  return copied;
}

test("startup failures remove the short temporary directory", (t) => {
  for (const mode of ["cache-directory", "initial-lock-read"] as const) {
    const root = fixture(t);
    const copied = isolatedCLI(root);
    const marker = path.join(root, "temporary.txt");
    const preload = path.join(root, "trace-temporary.cjs");
    writeFileSync(
      preload,
      `
      const fs = require("node:fs");
      const original = fs.mkdtempSync;
      fs.mkdtempSync = function(prefix, ...args) {
        const result = original.call(this, prefix, ...args);
        if (prefix === "/tmp/dg-") fs.writeFileSync(${JSON.stringify(marker)}, result);
        return result;
      };
      require("node:module").syncBuiltinESMExports();
    `,
    );
    if (mode === "cache-directory")
      writeFileSync(path.join(root, "runner/.build"), "not a directory");
    else rmSync(path.join(root, "runner/go.sum"));
    const result = spawnSync(
      process.execPath,
      ["--require", preload, copied, "--stage=module"],
      {
        cwd: root,
        encoding: "utf8",
        timeout: 10_000,
      },
    );
    const temporary = readFileSync(marker, "utf8");
    try {
      assert.ifError(result.error);
      assert.equal(result.status, 1, result.stdout + result.stderr);
      assert.match(
        result.stderr,
        mode === "cache-directory" ? /ENOTDIR/ : /ENOENT.*go\.sum/,
      );
      assert.equal(existsSync(temporary), false, `Startup leaked ${temporary}`);
      assert.doesNotMatch(result.stdout, /Running go/);
    } finally {
      rmSync(temporary, { recursive: true, force: true });
    }
  }
});

test("stage failures preserve their diagnostics alongside changed or unreadable module locks", (t) => {
  for (const mode of ["lock-change", "lock-missing"] as const) {
    const root = fixture(t);
    const copied = isolatedCLI(root);
    const bin = path.join(root, "bin");
    mkdirSync(bin);
    writeFileSync(path.join(bin, "package.json"), '{"type":"commonjs"}\n');
    fakeGo(bin, mode);
    const result = spawnSync(process.execPath, [copied, "--stage=module"], {
      cwd: root,
      env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
      encoding: "utf8",
      timeout: 15_000,
    });
    assert.ifError(result.error);
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stderr, /go exited 1; log: .*\d+-go\.log/);
    assert.match(result.stderr, /target exited 3/);
    assert.match(result.stderr, /intentional test command failure/);
    assert.match(result.stderr, /Module lock verification failed for go\.sum/);
    assert.match(
      result.stderr,
      mode === "lock-change" ? /go\.sum changed/ : /ENOENT/,
    );
    assert.doesNotMatch(
      result.stdout,
      /Go stage passed|Complete Go check passed/,
    );
  }
});

test("preparation and check success are withheld until module locks pass final verification", (t) => {
  for (const mode of ["lock-change-success", "prepare-lock-change"] as const) {
    const root = fixture(t);
    const copied = isolatedCLI(root);
    const bin = path.join(root, "bin");
    mkdirSync(bin);
    writeFileSync(path.join(bin, "package.json"), '{"type":"commonjs"}\n');
    fakeGo(bin, mode);
    writeFileSync(
      path.join(bin, "docker"),
      `#!${process.execPath}\nprocess.exit(0);\n`,
      { mode: 0o755 },
    );
    const result = spawnSync(
      process.execPath,
      [copied, mode === "prepare-lock-change" ? "--prepare" : "--stage=module"],
      {
        cwd: root,
        env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
        encoding: "utf8",
        timeout: 15_000,
      },
    );
    assert.ifError(result.error);
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stderr, /go\.sum changed/);
    assert.doesNotMatch(
      result.stdout,
      /Complete Go check passed|Partial Go check passed|image prepared/,
    );
  }
});

test("create uncertainty stays unknown and lost replies are resolved against the original container", (t) => {
  const dockerExecutable = resolveExecutable("docker", process.env.PATH ?? "");
  for (const mode of [
    "create-unknown",
    "create-reply-lost",
    "rm-unknown",
  ] as const) {
    const root = fixture(t);
    fakeGo(root, "container-pass");
    const count = path.join(root, "create-count");
    writeFileSync(
      path.join(root, "docker"),
      `#!${process.execPath}
      const fs = require("node:fs");
      const {spawnSync} = require("node:child_process");
      const args = process.argv.slice(2);
      if(args[0] === "create") {
        fs.appendFileSync(${JSON.stringify(count)}, "attempt\\n");
        if(${JSON.stringify(mode)} === "create-unknown") {console.error("simulated lost create response");process.exit(1);}
      }
      const result = spawnSync(${JSON.stringify(dockerExecutable)}, args, {stdio: args[0] === "create" && ${JSON.stringify(mode)} === "create-reply-lost" ? "pipe" : "inherit"});
      if(args[0] === "create" && ${JSON.stringify(mode)} === "create-reply-lost" && result.status === 0) {console.error("simulated lost successful create response");process.exit(1);}
      if(args[0] === "rm" && ${JSON.stringify(mode)} === "rm-unknown" && result.status === 0) {console.error("simulated lost remove response");process.exit(1);}
      process.exit(result.status ?? 1);
    `,
      { mode: 0o755 },
    );
    const result = spawnSync(process.execPath, [cli, "--stage=container"], {
      cwd: repository,
      env: { ...process.env, PATH: `${root}:${process.env.PATH}` },
      encoding: "utf8",
      timeout: 20_000,
    });
    const name = /Private container handle: (designs-go-[a-f0-9-]+)/.exec(
      result.stdout ?? "",
    )?.[1];
    try {
      assert.ifError(result.error);
      assert.ok(name, result.stdout + result.stderr);
      assert.equal(
        readFileSync(count, "utf8").trim().split("\n").length,
        mode === "rm-unknown" ? 2 : 1,
      );
      if (mode === "create-unknown") {
        assert.equal(result.status, 1, result.stdout + result.stderr);
        assert.match(result.stderr, /creation outcome unknown/);
        assert.doesNotMatch(result.stdout, /Private container absent/);
      } else if (mode === "create-reply-lost") {
        assert.equal(result.status, 1, result.stdout + result.stderr);
        assert.match(
          result.stderr,
          /simulated lost successful create response/,
        );
        assert.match(
          result.stdout,
          /Container terminal: [a-f0-9]{64} created exit=0/,
        );
        assert.match(result.stdout, /Private container absent/);
        assert.doesNotMatch(
          result.stdout,
          /Running docker start|Go stage passed/,
        );
        const id = /Container terminal: ([a-f0-9]{64})/.exec(
          result.stdout,
        )?.[1];
        assert.ok(id);
        for (const filter of [`id=${id}`, `name=^/${name}$`])
          assert.equal(
            execFileSync(
              dockerExecutable,
              [
                "container",
                "ls",
                "--all",
                "--no-trunc",
                "--filter",
                filter,
                "--format",
                "{{.ID}}",
              ],
              { encoding: "utf8", timeout: 10_000 },
            ).trim(),
            "",
          );
      } else {
        assert.equal(result.status, 0, result.stdout + result.stderr);
        assert.match(
          result.stdout,
          /Removal confirmed by readback after client error/,
        );
        assert.match(result.stdout, /Private container absent/);
      }
    } finally {
      for (const match of result.stdout.matchAll(
        /Private container handle: (designs-go-[a-f0-9-]+)/g,
      ))
        cleanupContainer(match[1]!, dockerExecutable);
    }
  }
});

test("failed container wait retains terminal logs and reports log failures without replay", (t) => {
  const dockerExecutable = resolveExecutable("docker", process.env.PATH ?? "");
  for (const logFailure of [false, true]) {
    const root = fixture(t);
    fakeGo(root, "container-pass");
    const calls = path.join(root, "docker-calls");
    writeFileSync(
      path.join(root, "docker"),
      `#!${process.execPath}
      const fs = require("node:fs");
      const {spawnSync} = require("node:child_process");
      const args = process.argv.slice(2);
      fs.appendFileSync(${JSON.stringify(calls)}, args[0] + "\\n");
      if (args[0] === "logs" && ${JSON.stringify(logFailure)}) { console.error("intentional diagnostic read failure"); process.exit(8); }
      const result = spawnSync(${JSON.stringify(dockerExecutable)}, args, {stdio:args[0] === "wait" ? "pipe" : "inherit", timeout:10000});
      if (args[0] === "wait" && result.status === 0) { console.error("intentional wait response failure"); process.exit(9); }
      process.exit(result.status ?? 1);
    `,
      { mode: 0o755 },
    );
    const result = spawnSync(process.execPath, [cli, "--stage=container"], {
      cwd: repository,
      env: { ...process.env, PATH: `${root}:${process.env.PATH}` },
      encoding: "utf8",
      timeout: 20000,
    });
    const name = /Private container handle: (designs-go-[a-f0-9-]+)/.exec(
      result.stdout ?? "",
    )?.[1];
    try {
      assert.ifError(result.error);
      assert.ok(name, result.stdout + result.stderr);
      assert.equal(result.status, 1, result.stdout + result.stderr);
      assert.match(result.stderr, /intentional wait response failure/);
      if (logFailure) {
        assert.match(result.stderr, /Container diagnostics failed/);
        assert.match(result.stderr, /intentional diagnostic read failure/);
      } else {
        const work = /Go check logs and artifacts: (.+)/.exec(
          result.stdout,
        )?.[1];
        assert.ok(work, result.stdout);
        assert.match(
          readFileSync(
            path.join(work, "container-normal-test-output.txt"),
            "utf8",
          ),
          /PASS: TestDatabaseChildInPrivateContainer/,
        );
      }
      assert.match(result.stdout, /Private container absent/);
      assert.doesNotMatch(
        result.stdout,
        /Partial Go check passed|Complete Go check passed/,
      );
      const commands = readFileSync(calls, "utf8").trim().split("\n");
      for (const command of ["create", "start", "wait", "logs", "rm"])
        assert.equal(commands.filter((value) => value === command).length, 1);
      assert.equal(
        execFileSync(
          dockerExecutable,
          [
            "container",
            "ls",
            "--all",
            "--filter",
            `name=^/${name}$`,
            "--format",
            "{{.ID}}",
          ],
          { encoding: "utf8", timeout: 10000 },
        ).trim(),
        "",
      );
    } finally {
      if (name) cleanupContainer(name, dockerExecutable);
    }
  }
});

test("cancellation during container removal completes cleanup and cannot report success", async (t) => {
  const dockerExecutable = resolveExecutable("docker", process.env.PATH ?? "");
  for (const cancellation of ["SIGTERM", "deadline"] as const) {
    const root = fixture(t);
    fakeGo(root, "container-pass");
    const removal = path.join(root, "removal-started");
    const release = path.join(root, "release-removal");
    const cancel = path.join(root, "cancel-deadline");
    writeFileSync(
      path.join(root, "docker"),
      `#!${process.execPath}
      const fs = require("node:fs");
      const {spawnSync} = require("node:child_process");
      const args = process.argv.slice(2);
      const execute = () => {
        const result = spawnSync(${JSON.stringify(dockerExecutable)}, args, {stdio:"inherit", timeout:10000});
        process.exit(result.status ?? 1);
      };
      if (args[0] !== "rm") execute();
      fs.writeFileSync(${JSON.stringify(removal)}, args[1]);
      const deadline = setTimeout(() => {console.error("removal fixture timed out"); process.exit(1);}, 15000);
      const poll = setInterval(() => {
        if (!fs.existsSync(${JSON.stringify(release)})) return;
        clearInterval(poll); clearTimeout(deadline); execute();
      }, 10);
    `,
      { mode: 0o755 },
    );
    const preload = path.join(root, "deadline.cjs");
    writeFileSync(
      preload,
      `
      const fs = require("node:fs");
      const original = global.setTimeout;
      global.setTimeout = (callback, milliseconds, ...args) => {
        if (milliseconds !== 30 * 60000) return original(callback, milliseconds, ...args);
        const timer = setInterval(() => {
          if (!fs.existsSync(${JSON.stringify(cancel)})) return;
          clearInterval(timer);
          callback(...args);
        }, 10);
        return timer;
      };
    `,
    );
    const child = spawn(
      process.execPath,
      [
        ...(cancellation === "deadline" ? ["--require", preload] : []),
        cli,
        "--stage=container",
      ],
      {
        cwd: repository,
        env: { ...process.env, PATH: `${root}:${process.env.PATH}` },
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
    let output = "";
    let errors = "";
    child.stdout.on("data", (chunk) => {
      output += chunk;
    });
    child.stderr.on("data", (chunk) => {
      errors += chunk;
    });
    const finished = new Promise<number | null>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", resolve);
    });
    const timeout = setTimeout(() => child.kill("SIGKILL"), 25000);
    let name: string | undefined;
    try {
      const waiting = Date.now() + 10000;
      while (
        !existsSync(removal) &&
        child.exitCode === null &&
        Date.now() < waiting
      )
        await delay(10);
      assert.ok(existsSync(removal), output + errors);
      name = /Private container handle: (designs-go-[a-f0-9-]+)/.exec(
        output,
      )?.[1];
      assert.ok(name, output + errors);
      const id = readFileSync(removal, "utf8");
      assert.match(id, /^[a-f0-9]{64}$/);
      if (cancellation === "SIGTERM") child.kill("SIGTERM");
      else writeFileSync(cancel, "cancel\n");
      const notice =
        cancellation === "SIGTERM"
          ? "Received SIGTERM"
          : "Go command deadline reached";
      const observed = Date.now() + 5000;
      while (
        !errors.includes(notice) &&
        child.exitCode === null &&
        Date.now() < observed
      )
        await delay(10);
      assert.ok(errors.includes(notice), output + errors);
      writeFileSync(release, "continue cleanup\n");
      assert.equal(await finished, 130, output + errors);
      assert.match(output, /Container terminal:/);
      assert.match(output, /Private container absent:/);
      assert.doesNotMatch(
        output,
        /Complete Go check passed|Partial Go check passed|image prepared/,
      );
      for (const filter of [`id=${id}`, `name=^/${name}$`])
        assert.equal(
          execFileSync(
            dockerExecutable,
            [
              "container",
              "ls",
              "--all",
              "--no-trunc",
              "--filter",
              filter,
              "--format",
              "{{.ID}}",
            ],
            { encoding: "utf8", timeout: 10000 },
          ).trim(),
          "",
        );
    } finally {
      writeFileSync(release, "continue cleanup\n");
      if (child.exitCode === null && child.signalCode === null)
        child.kill("SIGTERM");
      await finished;
      clearTimeout(timeout);
      name ??= /Private container handle: (designs-go-[a-f0-9-]+)/.exec(
        output,
      )?.[1];
      if (name) cleanupContainer(name, dockerExecutable);
    }
  }
});

test("CLI cancellation and forced termination leave no owned Docker fixture", async (t) => {
  const dockerExecutable = resolveExecutable("docker", process.env.PATH ?? "");
  for (const signal of ["SIGTERM", "SIGKILL"] as const) {
    const root = fixture(t);
    fakeGo(root, "container");
    const child = spawn(process.execPath, [cli, "--stage=container"], {
      cwd: repository,
      env: { ...process.env, PATH: `${root}:${process.env.PATH}` },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    let errors = "";
    child.stdout.on("data", (chunk) => {
      output += chunk;
    });
    child.stderr.on("data", (chunk) => {
      errors += chunk;
    });
    const finished = new Promise<number | null>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", resolve);
    });
    const deadline = setTimeout(() => child.kill("SIGKILL"), 30_000);
    let name: string | undefined;
    try {
      const deadline = Date.now() + 15_000;
      while (
        !output.includes("Running docker wait") &&
        Date.now() < deadline &&
        child.exitCode === null
      )
        await delay(25);
      name = /Private container handle: (designs-go-[a-f0-9-]+)/.exec(
        output,
      )?.[1];
      assert.ok(name, output + errors);
      assert.ok(output.includes("Running docker wait"), output + errors);
      const network = execFileSync(
        dockerExecutable,
        ["inspect", "--format", "{{.HostConfig.NetworkMode}}", name],
        { encoding: "utf8", timeout: 10_000 },
      ).trim();
      assert.equal(network, "none");
      if (signal === "SIGTERM") {
        let logs = "";
        const logging = Date.now() + 5000;
        while (
          !logs.includes("private container started before cancellation") &&
          Date.now() < logging
        ) {
          logs = execFileSync(dockerExecutable, ["logs", name], {
            encoding: "utf8",
            timeout: 10000,
          });
          if (!logs.includes("private container started before cancellation"))
            await delay(10);
        }
        assert.match(logs, /private container started before cancellation/);
      }
      child.kill(signal);
      const status = await finished;
      if (signal === "SIGTERM") {
        assert.equal(status, 130, output + errors);
        assert.match(output, /Container terminal:/);
        assert.match(output, /Private container absent:/);
        const work = /Go check logs and artifacts: (.+)/.exec(output)?.[1];
        assert.ok(work, output);
        assert.match(
          readFileSync(
            path.join(work, "container-normal-test-output.txt"),
            "utf8",
          ),
          /private container started before cancellation/,
        );
        assert.equal(
          execFileSync(
            dockerExecutable,
            [
              "container",
              "ls",
              "--all",
              "--filter",
              `name=^/${name}$`,
              "--format",
              "{{.ID}}",
            ],
            { encoding: "utf8" },
          ).trim(),
          "",
        );
      } else {
        assert.equal(status, null, output + errors);
        assert.equal(child.signalCode, "SIGKILL");
      }
    } finally {
      if (child.exitCode === null && child.signalCode === null)
        child.kill("SIGTERM");
      await finished;
      clearTimeout(deadline);
      name ??= /Private container handle: (designs-go-[a-f0-9-]+)/.exec(
        output,
      )?.[1];
      if (name) cleanupContainer(name, dockerExecutable);
    }
  }
});
