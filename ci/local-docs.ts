import { execFile, spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import {
  appendFileSync,
  chmodSync,
  closeSync,
  copyFileSync,
  cpSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { listSourcePaths, readSourceIdentity } from "./source.ts";

const execute = promisify(execFile);
const usage =
  "node ci/local-docs.ts --image sha256:ID --shared-bundle FILE --shared-lock FILE --node FILE --lychee FILE --prose FILE --ids FILE --output NEW_DIRECTORY";
type Options = {
  image: string;
  "shared-bundle": string;
  "shared-lock": string;
  node: string;
  lychee: string;
  prose: string;
  ids: string;
  output: string;
};

export function parseOptions(args: string[]): Options {
  const values: Record<string, string> = {};
  const names = [
    "image",
    "shared-bundle",
    "shared-lock",
    "node",
    "lychee",
    "prose",
    "ids",
    "output",
  ];
  for (let index = 0; index < args.length; index += 2) {
    const key = args[index]?.slice(2),
      value = args[index + 1];
    if (
      !args[index]?.startsWith("--") ||
      !key ||
      !names.includes(key) ||
      Object.hasOwn(values, key) ||
      !value ||
      value.startsWith("--")
    )
      throw new Error(usage);
    values[key] = value;
  }
  if (
    names.some((name) => !values[name]) ||
    !/^sha256:[a-f0-9]{64}$/.test(values.image!)
  )
    throw new Error(usage);
  return values as Options;
}

async function docker(args: string[]): Promise<string> {
  return (
    await execute("docker", args, { timeout: 30_000, maxBuffer: 1024 * 1024 })
  ).stdout.trim();
}

type Container = {
  Id: string;
  Config: { Labels: Record<string, string> };
  State: {
    Status: string;
    Running: boolean;
    ExitCode: number;
    Error: string;
    OOMKilled: boolean;
  };
};

// The invocation label prevents a name from authorizing operations on another container.
export async function settleContainer(
  name: string,
  invocation: string,
  call: (args: string[]) => Promise<string>,
  record: (event: object) => void,
): Promise<Container> {
  const inspect = async () => {
    const value = JSON.parse(
      await call([
        "inspect",
        "--type",
        "container",
        "--format",
        "{{json .}}",
        name,
      ]),
    ) as Container;
    if (
      !/^[a-f0-9]{64}$/.test(value.Id) ||
      value.Config?.Labels?.["designs.local-docs"] !== invocation ||
      !value.State ||
      typeof value.State.Running !== "boolean"
    )
      throw new Error("Original container identity is unknown");
    return {
      Id: value.Id,
      Config: { Labels: { "designs.local-docs": invocation } },
      State: {
        Status: value.State.Status,
        Running: value.State.Running,
        ExitCode: value.State.ExitCode,
        Error: value.State.Error,
        OOMKilled: value.State.OOMKilled,
      },
    };
  };
  let container = await inspect();
  if (container.State.Running) {
    record({ phase: "stop", outcome: "started", id: container.Id });
    try {
      await call(["stop", "--time", "5", container.Id]);
    } catch {
      record({ phase: "stop", outcome: "unknown", id: container.Id });
    }
    container = await inspect();
  }
  if (
    container.State.Running ||
    !["exited", "created"].includes(container.State.Status)
  )
    throw new Error(
      "Original container has not settled; preserve its identity",
    );
  record({ phase: "settled", id: container.Id, state: container.State });
  return container;
}

async function attach(name: string, log: string, signal: AbortSignal) {
  const descriptor = openSync(log, "wx", 0o600);
  try {
    return await new Promise<{
      code: number | null;
      signal: string | null;
      error: string | null;
    }>((resolve) => {
      const child = spawn("docker", ["start", "--attach", name], {
        stdio: ["ignore", descriptor, descriptor],
        signal,
      });
      let error: string | null = null;
      child.on("error", (cause) => {
        error = cause.message;
      });
      child.on("close", (code, exitSignal) =>
        resolve({ code, signal: exitSignal, error }),
      );
    });
  } finally {
    closeSync(descriptor);
  }
}

export async function localDocs(options: Options): Promise<string> {
  const root = realpathSync(path.resolve(import.meta.dirname, ".."));
  const manifest = JSON.parse(
    readFileSync(path.join(root, "package.json"), "utf8"),
  ) as { engines: { node: string } };
  if (
    process.platform !== "linux" ||
    process.versions.node !== manifest.engines.node
  )
    throw new Error(
      "Local docs acceptance requires Linux and the repository's pinned Node",
    );
  if (
    (await docker([
      "image",
      "inspect",
      "--format",
      "{{.Id}}",
      options.image,
    ])) !== options.image
  )
    throw new Error("The exact local acceptance image is unavailable");
  const output = path.resolve(options.output);
  if (
    output === root ||
    output.startsWith(root + path.sep) ||
    realpathSync(path.dirname(output)) !== path.dirname(output) ||
    output.includes(",")
  )
    throw new Error(
      "Output must be a new physical directory outside the source checkout, without commas",
    );
  if (!lstatSync(path.join(root, "node_modules")).isDirectory())
    throw new Error(
      "Prepare a real local dependency installation with the frozen lock first",
    );
  const files = {
    node: options.node,
    lychee: options.lychee,
    "check-prose.mjs": options.prose,
    "check-ids.mjs": options.ids,
  };
  const hash = (file: string) =>
    createHash("sha256").update(readFileSync(file)).digest("hex");
  const tools = Object.entries(files).map(([name, file]) => {
    const source = realpathSync(file);
    if (!lstatSync(source).isFile())
      throw new Error(`Expected tool file: ${name}`);
    return { name, source, sha256: hash(source) };
  });
  const distribution = [options["shared-bundle"], options["shared-lock"]].map(
    (file) => {
      const source = realpathSync(file);
      if (!lstatSync(source).isFile())
        throw new Error("Expected a regular shared input");
      return { source, sha256: hash(source) };
    },
  );
  const original = readSourceIdentity(root, listSourcePaths(root));
  const invocation = randomUUID(),
    name = `designs-docs-${invocation}`;
  mkdirSync(output, { mode: 0o700 });
  const record = (event: object) =>
    appendFileSync(
      path.join(output, "events.jsonl"),
      JSON.stringify({ time: new Date().toISOString(), ...event }) + "\n",
      { mode: 0o600 },
    );
  writeFileSync(
    path.join(output, "receipt.json"),
    JSON.stringify(
      {
        kind: "local-development-probe",
        invocation,
        container: name,
        image: options.image,
        root,
        source: original,
        tools,
        distribution,
      },
      null,
      2,
    ) + "\n",
    { flag: "wx", mode: 0o600 },
  );
  console.log(
    JSON.stringify({
      evidence: output,
      container: name,
      kind: "local-development-probe",
    }),
  );

  const snapshot = path.join(output, "source"),
    toolRoot = path.join(output, "tools"),
    evidence = path.join(output, "evidence");
  for (const directory of [snapshot, toolRoot, evidence]) mkdirSync(directory);
  for (const entry of original.entries) {
    const target = path.join(snapshot, entry.path);
    mkdirSync(path.dirname(target), { recursive: true });
    copyFileSync(path.join(root, entry.path), target);
    chmodSync(target, entry.mode & 0o111 ? 0o755 : 0o644);
    if (hash(target) !== entry.sha256)
      throw new Error("Source changed while preparing the snapshot");
  }
  cpSync(path.join(root, "node_modules"), path.join(snapshot, "node_modules"), {
    recursive: true,
    verbatimSymlinks: true,
  });
  if (
    readSourceIdentity(root, listSourcePaths(root)).sha256 !== original.sha256
  )
    throw new Error("Source changed while preparing local acceptance");
  for (const tool of tools) {
    const target = path.join(toolRoot, tool.name);
    copyFileSync(tool.source, target);
    chmodSync(target, tool.name.endsWith(".mjs") ? 0o644 : 0o755);
    if (hash(target) !== tool.sha256)
      throw new Error("Tool changed while preparing local acceptance");
  }
  const sharedInput = path.join(output, "shared-input");
  mkdirSync(sharedInput);
  for (const [index, input] of distribution.entries()) {
    const target = path.join(
      sharedInput,
      index === 0 ? "shared.bundle" : "shared.lock.json",
    );
    copyFileSync(input.source, target);
    chmodSync(target, 0o644);
    if (hash(target) !== input.sha256)
      throw new Error("Shared input changed while copying");
  }

  const args = [
    "create",
    "--pull=never",
    "--init",
    "--name",
    name,
    "--label",
    `designs.local-docs=${invocation}`,
    "--network",
    "none",
    "--read-only",
    "--cap-drop",
    "ALL",
    ...["SETUID", "SETGID", "KILL", "CHOWN", "FOWNER", "DAC_OVERRIDE"].flatMap(
      (cap) => ["--cap-add", cap],
    ),
    "--security-opt",
    "no-new-privileges",
    "--user",
    "0",
    "--tmpfs",
    "/tmp:rw,exec,mode=1777",
    "--tmpfs",
    "/work:rw,exec,mode=755,size=2g",
    "--mount",
    `type=bind,src=${snapshot},dst=/source,readonly`,
    "--mount",
    `type=bind,src=${toolRoot},dst=/host-tools,readonly`,
    "--mount",
    `type=bind,src=${toolRoot},dst=/host-docs,readonly`,
    "--mount",
    `type=bind,src=${sharedInput},dst=/shared-input,readonly`,
    "--mount",
    `type=bind,src=${evidence},dst=/evidence`,
    "--entrypoint",
    "/host-tools/node",
    options.image,
    "/source/ci/designs-acceptance.mjs",
    "/shared-input/shared.bundle",
    "/shared-input/shared.lock.json",
  ];
  writeFileSync(
    path.join(output, "command.json"),
    JSON.stringify(["docker", ...args], null, 2) + "\n",
    { flag: "wx", mode: 0o600 },
  );
  const controller = new AbortController();
  const cancel = () => controller.abort();
  process.on("SIGINT", cancel);
  process.on("SIGTERM", cancel);
  const timer = setTimeout(cancel, 600_000);
  let error: unknown,
    attached: Awaited<ReturnType<typeof attach>> | undefined,
    container: Container | undefined;
  try {
    record({ phase: "create", outcome: "started" });
    const id = await docker(args);
    if (!/^[a-f0-9]{64}$/.test(id))
      throw new Error("Container creation result is unknown");
    record({ phase: "create", outcome: "complete", id });
    if (controller.signal.aborted)
      throw new Error("Cancelled before starting the original container");
    record({ phase: "start", outcome: "started", id });
    attached = await attach(
      name,
      path.join(output, "container.log"),
      controller.signal,
    );
  } catch (cause) {
    error = cause;
  } finally {
    clearTimeout(timer);
    try {
      container = await settleContainer(name, invocation, docker, record);
    } catch (cause) {
      error ??= cause;
      record({ phase: "settlement", outcome: "unknown" });
    }
    process.off("SIGINT", cancel);
    process.off("SIGTERM", cancel);
  }
  const passed =
    !error &&
    !controller.signal.aborted &&
    attached?.code === 0 &&
    !attached.signal &&
    !attached.error &&
    container?.State.Status === "exited" &&
    container.State.ExitCode === 0 &&
    !container.State.OOMKilled &&
    !container.State.Error;
  writeFileSync(
    path.join(output, "result.json"),
    JSON.stringify(
      {
        passed,
        cancelled: controller.signal.aborted,
        attached,
        container,
        error:
          error instanceof Error ? error.message : error ? String(error) : null,
      },
      null,
      2,
    ) + "\n",
    { flag: "wx", mode: 0o600 },
  );
  if (!passed)
    throw new Error(
      `Local docs acceptance failed; preserve ${output} and container ${name}`,
    );
  return output;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  if (process.argv.length === 3 && process.argv[2] === "--help")
    console.log(usage);
  else {
    try {
      await localDocs(parseOptions(process.argv.slice(2)));
    } catch (error) {
      console.error(error instanceof Error ? error.message : String(error));
      process.exitCode = 1;
    }
  }
}
