import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import test, { type TestContext } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { parseMode, runCommand, verifyTestEvents } from "./go-check.ts";

const repository = path.resolve(import.meta.dirname, "..");
const cli = path.join(import.meta.dirname, "go-check.ts");
const lifecycle = "github.com/headironc/designs/runner/internal/lifecycle";
const processTest = "TestActualProcessIdentityAndExit";
const privateTest = "TestDatabaseChildInPrivateContainer";

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

function fakeGo(
  root: string,
  mode: "failure" | "skip" | "container" | "container-pass",
): void {
  const script = `#!${process.execPath}
    const fs = require("node:fs");
    const args = process.argv.slice(2);
    if(Object.keys(process.env).some(key => key.startsWith("RUNNER_")) || process.env.GOROOT || process.env.GOFLAGS !== "-mod=readonly" || process.env.GOPROXY !== "off") { console.error("ambient Go or fixture settings leaked"); process.exit(91); }
    if (args[0] === "version") { console.log("go version go1.27.3 linux/amd64"); process.exit(0); }
    if (args[0] === "test" && args.includes("-c")) {
      const body = ${JSON.stringify(mode)} === "container-pass"
        ? "#!/bin/sh\\nprintf '=== RUN   TestDatabaseChildInPrivateContainer\\n--- PASS: TestDatabaseChildInPrivateContainer (0.00s)\\nPASS\\n'\\n"
        : "#!/bin/sh\\nexec sleep 120\\n";
      fs.writeFileSync(args[args.indexOf("-o")+1], body, {mode:0o755}); process.exit(0);
    }
    if (args[0] === "tool") {
      const result = require("node:child_process").spawnSync("go", args, {env:{...process.env,PATH:${JSON.stringify(process.env.PATH)}},stdio:"inherit"});
      process.exit(result.status ?? 1);
    }
    if (args[0] === "test") {
      if (${JSON.stringify(mode)} === "failure") { console.error("intentional test command failure"); process.exit(3); }
      process.stdout.write(${JSON.stringify(events("pass", true))}); process.exit(0);
    }
    process.exit(0);
  `;
  writeFileSync(path.join(root, "go"), script, { mode: 0o755 });
}

// Independent teardown also runs when an assertion or the CLI's cleanup fails.
function cleanupContainer(name: string): void {
  const docker = (args: string[]) =>
    execFileSync("/usr/bin/docker", args, {
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

test("create uncertainty stays unknown and a lost removal reply is resolved by readback", (t) => {
  for (const mode of ["create-unknown", "rm-unknown"] as const) {
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
      const result = spawnSync("/usr/bin/docker", args, {stdio:"inherit"});
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
      assert.equal(readFileSync(count, "utf8").trim().split("\n").length, 1);
      if (mode === "create-unknown") {
        assert.equal(result.status, 1, result.stdout + result.stderr);
        assert.match(result.stderr, /creation outcome unknown/);
        assert.doesNotMatch(result.stdout, /Private container absent/);
      } else {
        assert.equal(result.status, 0, result.stdout + result.stderr);
        assert.match(
          result.stdout,
          /Removal confirmed by readback after client error/,
        );
        assert.match(result.stdout, /Private container absent/);
      }
    } finally {
      if (name) cleanupContainer(name);
    }
  }
});

test("CLI cancellation and forced termination leave no owned Docker fixture", async (t) => {
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
        "docker",
        ["inspect", "--format", "{{.HostConfig.NetworkMode}}", name],
        { encoding: "utf8", timeout: 10_000 },
      ).trim();
      assert.equal(network, "none");
      child.kill(signal);
      const status = await finished;
      if (signal === "SIGTERM") {
        assert.equal(status, 130, output + errors);
        assert.match(output, /Container terminal:/);
        assert.match(output, /Private container absent:/);
        assert.equal(
          execFileSync(
            "docker",
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
      if (name) cleanupContainer(name);
    }
  }
});
