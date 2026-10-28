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

const repository = path.resolve(import.meta.dirname, "..");
const runner = path.join(repository, "runner");
const lifecycle = "github.com/headironc/designs/runner/internal/lifecycle";
const firewall = "github.com/headironc/designs/runner/internal/firewall";
const privateTest = "TestDatabaseChildInPrivateContainer";
const processTest = "TestActualProcessIdentityAndExit";
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
  },
): Promise<string> {
  if (options.signal?.aborted)
    throw new CommandFailure("cancelled before starting command", false);
  const log = openSync(options.log, "wx", 0o600);
  const child = (() => {
    // Go fixtures assert exact requested file modes, independent of caller umask.
    const previous = process.umask(0o022);
    try {
      return spawn(command, args, {
        cwd: options.cwd,
        env: options.env,
        detached: true,
        stdio: ["pipe", "pipe", "pipe"],
      });
    } catch (error) {
      closeSync(log);
      throw new CommandFailure(String(error), false);
    } finally {
      process.umask(previous);
    }
  })();
  let reason: string | undefined;
  let killTimer: NodeJS.Timeout | undefined;
  let size = 0;
  const output: Buffer[] = [];
  let stderrTail = "";
  const stop = (message: string) => {
    if (reason) return;
    reason = message;
    if (child.pid) {
      signalGroup(child.pid, "SIGTERM");
      killTimer = setTimeout(() => signalGroup(child.pid!, "SIGKILL"), 2_000);
    }
  };
  const abort = () => stop("cancelled");
  options.signal?.addEventListener("abort", abort, { once: true });
  const timeout = setTimeout(
    () => stop(`timed out after ${options.timeoutMs} ms`),
    options.timeoutMs,
  );
  child.stdout.on("data", (chunk: Buffer) => {
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
  child.stderr.on("data", (chunk: Buffer) => {
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
    if (child.pid && groupAlive(child.pid))
      stop("leader exited with surviving process group");
  });
  child.stdin.on("error", () => {}); // Early child exit is handled by its final status.
  child.stdin.end(options.input);
  try {
    const status = await new Promise<{
      code: number | null;
      signal: string | null;
    }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code, signal) => resolve({ code, signal }));
    });
    if (child.pid) {
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
    if (reason || status.code !== 0 || status.signal) {
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
        `${command} ${reason ?? `exited ${status.code ?? status.signal}`}; log: ${options.log}${detail ? "\n" + detail : ""}`,
        child.pid !== undefined,
      );
    }
    return Buffer.concat(output).toString("utf8");
  } catch (error) {
    if (error instanceof CommandFailure) throw error;
    throw new CommandFailure(
      `${String(error)}; log: ${options.log}`,
      child.pid !== undefined,
      child.pid === undefined || !groupAlive(child.pid),
    );
  } finally {
    clearTimeout(timeout);
    clearTimeout(killTimer);
    options.signal?.removeEventListener("abort", abort);
    closeSync(log);
  }
}

/** Check Go's actual event stream; an omitted, skipped or duplicate required test fails. */
export function verifyTestEvents(
  raw: string,
  stage: Stage,
  packagesWithoutTests: string[] = [],
): void {
  const required =
    stage === "module"
      ? [
          [lifecycle, processTest, "pass"],
          [lifecycle, privateTest, "skip"],
        ]
      : stage === "container"
        ? [[lifecycle, privateTest, "pass"]]
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
        event.Test === privateTest
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
      ([key]) => !key.startsWith("GO") && !key.startsWith("RUNNER_"),
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
          { timeoutMs: 1_800_000 },
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
        const executable = path.join(work, "lifecycle.test");
        await run(
          "go",
          ["test", "-c", "-o", executable, "./internal/lifecycle"],
          { env: { ...env, CGO_ENABLED: "0" }, timeoutMs: 600_000 },
        );
        const name = `designs-go-${randomUUID()}`;
        writeFileSync(path.join(work, "container-name.txt"), name + "\n", {
          mode: 0o600,
        });
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
            await docker(["inspect", "--format", "{{json .State}}", id], true),
          ) as { Running: boolean; Status: string; ExitCode: number };
        let createStarted = false;
        let createConfirmed = false;
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
              "printf 'nucleiron:x:24568:\\n' >> /etc/group\nprintf 'nucleiron:x:24567:24568::/nonexistent:/bin/false\\n' >> /etc/passwd\ncp /input/lifecycle.test /tmp/lifecycle.test\nchmod 755 /tmp/lifecycle.test\nexec /tmp/lifecycle.test -test.v=test2json -test.timeout=30s -test.run=^TestDatabaseChildInPrivateContainer$",
            ]);
            createConfirmed = true;
          } catch (error) {
            if (error instanceof CommandFailure && !error.started)
              createStarted = false;
            throw error;
          }
          await docker(["start", name]);
          const exit = (await docker(["wait", name])).trim();
          const raw = await docker(["logs", name]);
          writeFileSync(path.join(work, "container-test-output.txt"), raw, {
            mode: 0o600,
          });
          const events = await run(
            "go",
            ["tool", "test2json", "-p", lifecycle],
            { input: raw },
          );
          verifyTestEvents(events, stage);
          if (exit !== "0")
            throw new Error(`Private container ${name} exited ${exit}`);
        } finally {
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
