import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import type { Readable } from "node:stream";

const repository = path.resolve(import.meta.dirname, "..");
const runner = path.join(repository, "runner");
const lifecycle = "github.com/headironc/designs/runner/internal/lifecycle";
const firewall = "github.com/headironc/designs/runner/internal/firewall";
const privateTest = "TestDatabaseChildInPrivateContainer";
const processTest = "TestActualProcessIdentityAndExit";
const installedGroups = {
  TestInstalledFileStates: [
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
  ],
  TestInstalledInputsBlockFileReads: [
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
  ],
  TestInstalledConcurrentChangesAndCancellation: [
    "replace",
    "same_size",
    "appear",
    "xattr",
    "parent_replace",
    "plan_change",
    "lock_replace",
    "cancel",
    "fifo_open_race",
  ],
};
const installedRoots = [
  "TestInstalledHistoricalMatchAndReadOnlyReentry",
  ...Object.entries(installedGroups).flatMap(([group, names]) =>
    names.map((name) => `${group}/${name}`),
  ),
];
const installedOrdinary = [
  ...Object.keys(installedGroups),
  "TestInstalledStreamingBoundsAndPrivateFiles",
  "TestInstalledCLIAndOutputBoundary",
];
// Official Ubuntu 26.04 linux/amd64 manifest, consumed only by the private fixture.
const image =
  "ubuntu@sha256:157dc3d6e4a3e4f2c68d12df9df9ed6e808b59af6d4c3757b432b4f519916725";
const stages = ["module", "container", "nft"] as const;
type Stage = (typeof stages)[number];

export class CommandFailure extends Error {
  readonly started: boolean;
  readonly groupClosed: boolean;
  constructor(message: string, started: boolean, groupClosed = true) {
    super(message);
    this.started = started;
    this.groupClosed = groupClosed;
  }
}

function groupAlive(pid: number): boolean {
  try {
    process.kill(-pid, 0);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw error;
  }
}

function signalGroup(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(-pid, signal);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
}

/** Own the process group until it is gone, including on timeout or cancellation. */
export async function runCommand(
  command: string,
  args: string[],
  options: {
    cwd: string;
    env: NodeJS.ProcessEnv;
    log: string;
    timeoutMs: number;
    signal?: AbortSignal;
    input?: string;
    supervisor?: string;
  },
): Promise<string> {
  if (options.signal?.aborted)
    throw new CommandFailure("cancelled before starting command", false);
  const log = openSync(options.log, "wx", 0o600);
  let receiptLog: number | undefined;
  try {
    if (options.supervisor)
      receiptLog = openSync(`${options.log}.supervisor.log`, "wx", 0o600);
  } catch (error) {
    closeSync(log);
    throw error;
  }
  const child = (() => {
    // Go fixtures assert exact requested file modes, independent of caller umask.
    const previous = process.umask(0o022);
    try {
      return spawn(
        options.supervisor ?? command,
        options.supervisor ? [command, ...args] : args,
        {
          cwd: options.cwd,
          env: options.env,
          detached: true,
          stdio: options.supervisor
            ? ["pipe", "pipe", "pipe", "pipe"]
            : ["pipe", "pipe", "pipe"],
        },
      );
    } catch (error) {
      closeSync(log);
      if (receiptLog !== undefined) closeSync(receiptLog);
      throw new CommandFailure(String(error), false);
    } finally {
      process.umask(previous);
    }
  })();
  let reason: string | undefined;
  let forced = false;
  let supervisorClosed = false;
  let targetStarted = false;
  const receipt = child.stdio[3] as Readable | undefined;
  const receiptChunks: Buffer[] = [];
  let receiptSize = 0;
  let killTimer: NodeJS.Timeout | undefined;
  let size = 0;
  const output: Buffer[] = [];
  let stderrTail = "";
  const forceSupervisor = () => {
    forced = true;
    child.kill("SIGKILL");
    // An unknown surviving descendant may still hold the output descriptors.
    child.stdout?.destroy();
    child.stderr?.destroy();
    child.stdin?.destroy();
    receipt?.destroy();
  };
  const stop = (message: string) => {
    if (reason) return;
    reason = message;
    if (child.pid) {
      if (options.supervisor) {
        child.kill("SIGTERM");
        // The helper owns an eight-second cleanup deadline.
        killTimer = setTimeout(forceSupervisor, 12_000);
      } else {
        signalGroup(child.pid, "SIGTERM");
        killTimer = setTimeout(() => signalGroup(child.pid!, "SIGKILL"), 2_000);
      }
    }
  };
  const abort = () => stop("cancelled");
  options.signal?.addEventListener("abort", abort, { once: true });
  const timeout = setTimeout(
    () => stop(`timed out after ${options.timeoutMs} ms`),
    options.timeoutMs,
  );
  receipt?.on("data", (chunk: Buffer) => {
    receiptSize += chunk.length;
    if (receiptSize > 16 * 1024) {
      stop("supervisor receipt exceeds 16 KiB");
      return;
    }
    receiptChunks.push(chunk);
    try {
      writeFileSync(receiptLog!, chunk);
    } catch (error) {
      stop(`supervisor receipt log failed: ${String(error)}`);
    }
  });
  child.stdout!.on("data", (chunk: Buffer) => {
    if (reason) return;
    size += chunk.length;
    if (size > 64 * 1024 * 1024) {
      stop("output exceeds 64 MiB");
      return;
    }
    output.push(chunk);
    try {
      writeFileSync(log, chunk);
    } catch (error) {
      stop(`log write failed: ${String(error)}`);
    }
  });
  child.stderr!.on("data", (chunk: Buffer) => {
    if (reason) return;
    size += chunk.length;
    stderrTail = (stderrTail + chunk.toString("utf8")).slice(-4096);
    if (size > 64 * 1024 * 1024) {
      stop("output exceeds 64 MiB");
      return;
    }
    try {
      writeFileSync(log, chunk);
    } catch (error) {
      stop(`log write failed: ${String(error)}`);
    }
  });
  // A completed leader must not leave descendants holding pipes or doing work.
  child.once("exit", () => {
    if (options.supervisor) {
      clearTimeout(killTimer);
      killTimer = setTimeout(forceSupervisor, 1_000);
      return;
    }
    if (child.pid && groupAlive(child.pid))
      stop("leader exited with surviving process group");
  });
  child.stdin!.on("error", () => {}); // Early child exit is handled by its final status.
  child.stdin!.end(options.input);
  try {
    const status = await new Promise<{
      code: number | null;
      signal: string | null;
    }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code, signal) => resolve({ code, signal }));
    });
    let supervisorFailure = "";
    if (options.supervisor) {
      try {
        const value: unknown = JSON.parse(
          Buffer.concat(receiptChunks).toString("utf8"),
        );
        if (!Array.isArray(value) || value.length !== 6)
          throw new Error("expected one six-field supervisor receipt");
        const [version, pid, exitCode, signal, closed, failures] = value;
        const integer = (number: unknown, minimum: number, maximum: number) =>
          Number.isInteger(number) &&
          (number as number) >= minimum &&
          (number as number) <= maximum;
        if (
          version !== 1 ||
          !integer(pid, 0, 2 ** 31 - 1) ||
          !(exitCode === null || integer(exitCode, 0, 255)) ||
          !(signal === null || integer(signal, 1, 64)) ||
          typeof closed !== "boolean" ||
          !Array.isArray(failures) ||
          failures.some((failure) => typeof failure !== "string") ||
          (exitCode !== null && signal !== null) ||
          (pid === 0 &&
            (exitCode !== null || signal !== null || failures.length === 0)) ||
          (pid > 0 && closed && exitCode === null && signal === null)
        )
          throw new Error("invalid supervisor receipt fields");
        const successful =
          closed && pid > 0 && exitCode === 0 && failures.length === 0;
        if (
          status.signal !== null ||
          status.code !== (successful ? 0 : 1) ||
          forced ||
          receiptSize > 16 * 1024
        )
          throw new Error("supervisor exit contradicts its receipt");
        supervisorClosed = closed;
        targetStarted = pid > 0;
        supervisorFailure = [
          ...failures,
          ...(exitCode !== null && exitCode !== 0
            ? [`target exited ${exitCode}`]
            : []),
          ...(signal !== null ? [`target received signal ${signal}`] : []),
        ].join("; ");
        if (!closed)
          throw new Error(supervisorFailure || "supervisor cleanup is unknown");
      } catch (error) {
        throw new CommandFailure(
          `${reason ? reason + "; " : ""}Supervisor cleanup unknown: ${String(error)}; supervisor PID ${child.pid}; retain temporary files and inspect original logs: ${options.log}`,
          targetStarted,
          false,
        );
      }
    } else if (child.pid) {
      const deadline = Date.now() + 3_000;
      while (groupAlive(child.pid) && Date.now() < deadline) {
        signalGroup(child.pid, "SIGKILL");
        await delay(25);
      }
      if (groupAlive(child.pid))
        throw new Error(
          `Process group ${child.pid} still exists; inspect it before retrying. Log: ${options.log}`,
        );
    }
    if (reason || status.code !== 0 || status.signal || supervisorFailure) {
      const lines = Buffer.concat(output).toString("utf8").split("\n");
      const failed = lines.findIndex((line) =>
        line.includes('"Action":"fail"'),
      );
      const detail = (
        failed >= 0
          ? lines.slice(Math.max(0, failed - 3), failed + 1).join("\n")
          : stderrTail || lines.slice(-8).join("\n")
      )
        .slice(-4096)
        .trim();
      throw new CommandFailure(
        `${command} ${reason ?? `exited ${status.code ?? status.signal}`}; log: ${options.log}${supervisorFailure ? "\n" + supervisorFailure : ""}${detail ? "\n" + detail : ""}`,
        options.supervisor ? targetStarted : child.pid !== undefined,
      );
    }
    return Buffer.concat(output).toString("utf8");
  } catch (error) {
    if (error instanceof CommandFailure) throw error;
    throw new CommandFailure(
      `${String(error)}; log: ${options.log}`,
      child.pid !== undefined,
      options.supervisor
        ? supervisorClosed
        : child.pid === undefined || !groupAlive(child.pid),
    );
  } finally {
    clearTimeout(timeout);
    clearTimeout(killTimer);
    options.signal?.removeEventListener("abort", abort);
    closeSync(log);
    if (receiptLog !== undefined) closeSync(receiptLog);
  }
}

/** Check Go's actual event stream; an omitted, skipped or duplicate required test fails. */
export function verifyTestEvents(
  raw: string,
  stage: Stage | "container-race",
  packagesWithoutTests: string[] = [],
): void {
  const required =
    stage === "module"
      ? [
          [lifecycle, processTest, "pass"],
          [lifecycle, privateTest, "skip"],
          ...installedRoots.map((name) => [lifecycle, name, "skip"]),
          ...installedOrdinary.map((name) => [lifecycle, name, "pass"]),
        ]
      : stage === "container" || stage === "container-race"
        ? [
            ...(stage === "container"
              ? [[lifecycle, privateTest, "pass"]]
              : []),
            ...[...installedRoots, ...installedOrdinary].map((name) => [
              lifecycle,
              name,
              "pass",
            ]),
          ]
        : [[firewall, "TestReloadInPrivateNamespace", "pass"]];
  const runs = new Map<string, number>();
  const finals = new Map<string, string[]>();
  let packagePasses = 0;
  for (const line of raw.split("\n").filter(Boolean)) {
    const event: { Action?: string; Package?: string; Test?: string } =
      JSON.parse(line);
    if (typeof event.Action !== "string" || typeof event.Package !== "string")
      throw new Error("Malformed Go test event");
    if (event.Action === "fail")
      throw new Error(`Go test failed: ${event.Package} ${event.Test ?? ""}`);
    if (!event.Test) {
      if (
        event.Action === "skip" &&
        !(stage === "module" && packagesWithoutTests.includes(event.Package))
      )
        throw new Error(`Unexpected skipped package: ${event.Package}`);
      if (event.Action === "pass") packagePasses++;
      continue;
    }
    const key = `${event.Package}/${event.Test}`;
    if (event.Action === "run") runs.set(key, (runs.get(key) ?? 0) + 1);
    if (["pass", "skip"].includes(event.Action))
      finals.set(key, [...(finals.get(key) ?? []), event.Action]);
    if (
      event.Action === "skip" &&
      !(
        stage === "module" &&
        event.Package === lifecycle &&
        (event.Test === privateTest || installedRoots.includes(event.Test))
      )
    )
      throw new Error(`Unexpected skipped test: ${key}`);
  }
  if (!packagePasses)
    throw new Error("No package passed in the Go event stream");
  for (const [pkg, name, action] of required) {
    const key = `${pkg}/${name}`;
    if (
      runs.get(key) !== 1 ||
      finals.get(key)?.length !== 1 ||
      finals.get(key)?.[0] !== action
    )
      throw new Error(
        `Required Go test did not ${action} exactly once: ${key}`,
      );
  }
}

export function parseMode(args: string[]): "prepare" | Stage[] {
  if (!args.length) return [...stages];
  if (args.length === 1 && args[0] === "--prepare") return "prepare";
  if (args.length === 1 && args[0]?.startsWith("--stage=")) {
    const stage = args[0].slice("--stage=".length);
    if (stages.includes(stage as Stage)) return [stage as Stage];
  }
  throw new Error(
    "Usage: bun run go:prepare | bun run go:check [--stage=module|container|nft]",
  );
}

function goEnvironment(prepare: boolean): NodeJS.ProcessEnv {
  // Fixture switches and ambient Go settings must not redirect the host tests.
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([key]) =>
        !["GO", "RUNNER_", "GUARD_", "WATCHDOG_STOP_"].some((prefix) =>
          key.startsWith(prefix),
        ),
    ),
  );
  return {
    ...env,
    GOTOOLCHAIN: "local",
    GOWORK: "off",
    GOENV: "off",
    GOFLAGS: "-mod=readonly",
    GOOS: "linux",
    GOARCH: "amd64",
    CGO_ENABLED: "1",
    GOPROXY: prepare ? "https://proxy.golang.org" : "off",
    GOSUMDB: prepare ? "sum.golang.org" : "off",
    GOCACHE: path.join(runner, ".build/go-cache"),
    GOMODCACHE: path.join(runner, ".build/go-mod"),
    GOTMPDIR: path.join(runner, ".build/go-tmp"),
  };
}

export async function checkGo(
  args: string[],
  signal: AbortSignal,
): Promise<void> {
  const mode = parseMode(args);
  if (process.platform !== "linux" || process.arch !== "x64")
    throw new Error("Go checks require Linux amd64");
  const env = goEnvironment(mode === "prepare");
  // Short real test paths avoid the Unix socket limit; build files stay on disk.
  const temporary = mkdtempSync("/tmp/dg-");
  let temporaryClosed = true;
  let locks: Buffer[] | undefined;
  const failures: unknown[] = [];
  try {
    env.TMPDIR = temporary;
    for (const name of ["GOCACHE", "GOMODCACHE", "GOTMPDIR"])
      mkdirSync(env[name]!, { recursive: true });
    const work = mkdtempSync(path.join(runner, ".build/go-check-"));
    console.log(`Go check logs and artifacts: ${work}`);
    locks = ["go.mod", "go.sum"].map((file) =>
      readFileSync(path.join(runner, file)),
    );
    let sequence = 0;
    const run = (
      command: string,
      commandArgs: string[],
      settings: {
        timeoutMs?: number;
        env?: NodeJS.ProcessEnv;
        cleanup?: boolean;
        input?: string;
        supervisor?: string;
      } = {},
    ) => {
      const label = `${String(++sequence).padStart(2, "0")}-${path.basename(command)}.log`;
      console.log(`Running ${command} ${commandArgs.join(" ")} (${label})`);
      return runCommand(command, commandArgs, {
        cwd: runner,
        env: settings.env ?? env,
        log: path.join(work, label),
        timeoutMs: settings.timeoutMs ?? 120_000,
        signal: settings.cleanup ? undefined : signal,
        input: settings.input,
        supervisor: settings.supervisor,
      }).catch((error) => {
        if (error instanceof CommandFailure && !error.groupClosed)
          temporaryClosed = false;
        throw error;
      });
    };
    const version = (await run("go", ["version"])).trim();
    const required = /^go (\S+)$/m.exec(locks[0]!.toString("utf8"))?.[1];
    if (!required || version !== `go version go${required} linux/amd64`)
      throw new Error(`Expected Go ${required} linux/amd64, got ${version}`);
    console.log(version);
    if (mode === "prepare") {
      await run("go", ["mod", "download"], { timeoutMs: 600_000 });
      await run("docker", ["pull", "--platform=linux/amd64", image], {
        timeoutMs: 600_000,
      });
      return;
    }
    for (const stage of mode) {
      signal.throwIfAborted();
      if (stage === "module") {
        if (process.getuid?.() === 0)
          throw new Error(
            "Module tests require a non-root host user; root fixtures run only in private containers",
          );
        await run("dbus-daemon", ["--version"]);
        await run("gcc", ["--version"]);
        await run("go", ["mod", "verify"]);
        const noTests = (
          await run("go", [
            "list",
            "-f",
            "{{if not (or .TestGoFiles .XTestGoFiles)}}{{.ImportPath}}{{end}}",
            "./...",
          ])
        )
          .split("\n")
          .filter(Boolean);
        const supervisor = path.join(work, "go-supervise");
        await run(
          "go",
          [
            "build",
            "-trimpath",
            "-o",
            supervisor,
            path.join(repository, "scripts/go-supervise.go"),
          ],
          {
            env: { ...env, CGO_ENABLED: "0" },
            timeoutMs: 120_000,
          },
        );
        const raw = await run(
          "go",
          [
            "test",
            `-exec=/usr/bin/env GOTMPDIR=${temporary}`,
            "-race",
            "-count=1",
            "-timeout=5m",
            "-json",
            "./...",
          ],
          { timeoutMs: 1_800_000, supervisor },
        );
        verifyTestEvents(raw, stage, noTests);
        await run("go", ["vet", "./..."], { timeoutMs: 600_000 });
        for (const command of [
          "runner-firewall",
          "runner-admin",
          "runner-bootstrap",
          "guarded-provider",
          "provider-watchdog",
        ])
          await run(
            "go",
            [
              "build",
              "-trimpath",
              "-o",
              path.join(work, command),
              `./cmd/${command}`,
            ],
            { env: { ...env, CGO_ENABLED: "0" }, timeoutMs: 600_000 },
          );
      } else if (stage === "container") {
        await run("docker", ["image", "inspect", image]);
        for (const variant of ["normal", "race"] as const) {
          const executable = path.join(work, `lifecycle-${variant}.test`);
          await run(
            "go",
            [
              "test",
              ...(variant === "race" ? ["-race"] : []),
              "-c",
              "-o",
              executable,
              "./internal/lifecycle",
            ],
            {
              env: { ...env, CGO_ENABLED: variant === "race" ? "1" : "0" },
              timeoutMs: 600_000,
            },
          );
          const name = `designs-go-${randomUUID()}`;
          writeFileSync(
            path.join(work, `container-${variant}-name.txt`),
            name + "\n",
            {
              mode: 0o600,
            },
          );
          console.log(`Private container handle: ${name}`);
          const docker = (values: string[], cleanup = false) =>
            run("docker", values, { timeoutMs: 30_000, cleanup });
          const findContainer = async () =>
            (
              await docker(
                [
                  "container",
                  "ls",
                  "--all",
                  "--no-trunc",
                  "--filter",
                  `name=^/${name}$`,
                  "--format",
                  "{{.ID}}",
                ],
                true,
              )
            ).trim();
          const state = async (id: string) =>
            JSON.parse(
              await docker(
                ["inspect", "--format", "{{json .State}}", id],
                true,
              ),
            ) as { Running: boolean; Status: string; ExitCode: number };
          let createStarted = false;
          let createConfirmed = false;
          let exit: string | undefined;
          let raw: string | undefined;
          const containerFailures: unknown[] = [];
          try {
            try {
              createStarted = true;
              await docker([
                "create",
                "--name",
                name,
                "--pull=never",
                "--network=none",
                "--mount",
                `type=bind,src=${executable},dst=/input/lifecycle.test,readonly`,
                "--env",
                "RUNNER_DIAGNOSTIC_PRIVATE_CONTAINER=1",
                image,
                "/bin/sh",
                "-eu",
                "-c",
                `printf 'nucleiron:x:24568:\\n' >> /etc/group\nprintf 'nucleiron:x:24567:24568::/nonexistent:/bin/false\\n' >> /etc/passwd\ncp /input/lifecycle.test /tmp/lifecycle.test\nchmod 755 /tmp/lifecycle.test\n${variant === "race" ? "/lib64/ld-linux-x86-64.so.2 --list /tmp/lifecycle.test >&2\n" : ""}exec /tmp/lifecycle.test -test.v=test2json -test.timeout=1m '-test.run=${variant === "race" ? "^TestInstalled" : "^(TestDatabaseChildInPrivateContainer$|TestInstalled)"}'`,
              ]);
              createConfirmed = true;
            } catch (error) {
              if (error instanceof CommandFailure && !error.started)
                createStarted = false;
              throw error;
            }
            await docker(["start", name]);
            exit = (await docker(["wait", name])).trim();
          } catch (error) {
            containerFailures.push(error);
          } finally {
            try {
              // Create/start can have succeeded even when their client returned an error.
              // Recover only this pre-recorded name; never repeat creation or start.
              const id = await findContainer();
              if (!id && createStarted && !createConfirmed)
                throw new Error(
                  `Container creation outcome unknown: ${name}; currently absent does not prove creation ended. Retain ${work} and resolve this original name before retrying.`,
                );
              if (id) {
                let current = await state(id);
                if (current.Running) {
                  try {
                    await docker(["stop", "--time=2", id], true);
                  } catch (error) {
                    console.error(
                      `Stop result requires readback for ${id}: ${String(error)}`,
                    );
                  }
                  current = await state(id);
                  if (current.Running) {
                    try {
                      await docker(["kill", id], true);
                    } catch (error) {
                      console.error(
                        `Kill result requires readback for ${id}: ${String(error)}`,
                      );
                    }
                    current = await state(id);
                  }
                }
                if (
                  current.Running ||
                  !["exited", "created", "dead"].includes(current.Status)
                )
                  throw new Error(
                    `Container ${id} is not terminal; retain ${work} and resolve it before retrying`,
                  );
                console.log(
                  `Container terminal: ${id} ${current.Status} exit=${current.ExitCode}`,
                );
                try {
                  raw = await docker(["logs", id], true);
                  writeFileSync(
                    path.join(work, `container-${variant}-test-output.txt`),
                    raw,
                    {
                      mode: 0o600,
                    },
                  );
                } catch (error) {
                  containerFailures.push(
                    new Error(
                      `Container diagnostics failed for ${name}/${id}: ${String(error)}`,
                      { cause: error },
                    ),
                  );
                }
                let removalError: unknown;
                try {
                  await docker(["rm", id], true);
                } catch (error) {
                  removalError = error;
                }
                const remainingId = (
                  await docker(
                    [
                      "container",
                      "ls",
                      "--all",
                      "--no-trunc",
                      "--filter",
                      `id=${id}`,
                      "--format",
                      "{{.ID}}",
                    ],
                    true,
                  )
                ).trim();
                if (remainingId || (await findContainer()))
                  throw new Error(
                    `Container ${name}/${id} removal unresolved: ${String(removalError ?? "still present")}; retain ${work}`,
                  );
                if (removalError)
                  console.log(
                    `Removal confirmed by readback after client error: ${id}`,
                  );
              }
              console.log(`Private container absent: ${name}`);
            } catch (error) {
              containerFailures.push(error);
            }
          }
          if (containerFailures.length === 1) throw containerFailures[0];
          if (containerFailures.length > 1)
            throw new AggregateError(
              containerFailures,
              containerFailures
                .map((error) =>
                  error instanceof Error ? error.message : String(error),
                )
                .join("\n"),
            );
          if (raw === undefined)
            throw new Error(`Container diagnostics unavailable: ${name}`);
          const events = await run(
            "go",
            ["tool", "test2json", "-p", lifecycle],
            {
              input: raw,
            },
          );
          verifyTestEvents(
            events,
            variant === "race" ? "container-race" : "container",
          );
          if (exit !== "0")
            throw new Error(`Private container ${name} exited ${exit}`);
          console.log(`Go container variant passed: ${variant}`);
        }
      } else {
        const raw = await run(
          "go",
          [
            "test",
            `-exec=/usr/bin/env GOTMPDIR=${temporary}`,
            "-count=1",
            "-tags=integration",
            "-run=^TestReloadInPrivateNamespace$",
            "-timeout=1m",
            "-json",
            "./internal/firewall",
          ],
          {
            env: { ...env, RUNNER_FIREWALL_NAMESPACE: "1" },
            timeoutMs: 600_000,
          },
        );
        verifyTestEvents(raw, stage);
      }
      console.log(`Go stage passed: ${stage}`);
    }
  } catch (error) {
    failures.push(error);
  } finally {
    try {
      if (temporaryClosed) rmSync(temporary, { recursive: true, force: true });
      else
        console.error(
          `Process cleanup unknown; retain temporary files at ${temporary}`,
        );
    } catch (error) {
      failures.push(
        new Error(
          `Temporary cleanup failed at ${temporary}: ${String(error)}`,
          { cause: error },
        ),
      );
    }
    if (locks) {
      for (const [index, file] of ["go.mod", "go.sum"].entries()) {
        try {
          if (!locks[index]!.equals(readFileSync(path.join(runner, file))))
            throw new Error(
              `${file} changed; inspect source changes before retrying`,
            );
        } catch (error) {
          failures.push(
            new Error(
              `Module lock verification failed for ${file}: ${String(error)}`,
              { cause: error },
            ),
          );
        }
      }
    }
    // Cleanup deliberately ignores cancellation, but must not turn it into success.
    if (signal.aborted && !failures.includes(signal.reason))
      failures.push(signal.reason);
    if (failures.length === 1) throw failures[0];
    if (failures.length > 1)
      throw new AggregateError(
        failures,
        failures
          .map((error) =>
            error instanceof Error ? error.message : String(error),
          )
          .join("\n"),
      );
    console.log(
      mode === "prepare"
        ? "Go dependencies and private test image prepared; no checks claimed."
        : mode.length === stages.length
          ? "Complete Go check passed: module, container, nft"
          : `Partial Go check passed: ${mode.join(", ")}; full acceptance requires module, container, nft`,
    );
  }
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const controller = new AbortController();
  const cancel = (signal: NodeJS.Signals) => {
    console.error(
      `Received ${signal}; terminating owned work and cleaning the private container`,
    );
    controller.abort(new Error(signal));
  };
  process.on("SIGINT", cancel);
  process.on("SIGTERM", cancel);
  const deadline = setTimeout(
    () => {
      console.error(
        "Go command deadline reached; stopping work and recovering owned resources",
      );
      controller.abort(new Error("Go command deadline"));
    },
    (process.argv.includes("--prepare") ? 20 : 30) * 60_000,
  );
  try {
    await checkGo(process.argv.slice(2), controller.signal);
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = controller.signal.aborted ? 130 : 1;
  } finally {
    clearTimeout(deadline);
    process.off("SIGINT", cancel);
    process.off("SIGTERM", cancel);
  }
}
