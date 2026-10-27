import path from "node:path";
import { BoundInputs } from "../.shared/ci/binding.ts";
import type { Check, Tool } from "../.shared/ci/contract.ts";
import { VerifiedExecution, verifyChecks } from "../.shared/ci/execution.ts";

function tool(inputs: BoundInputs, name: string, version: string): Tool {
  const result = inputs.paths.tools.find((entry) => entry.name === name);
  if (!result || result.version !== version)
    throw new Error(`Missing designs tool: ${name}`);
  return result;
}

/** Designs owns its actual document corpus and validation obligations. */
export function designsChecks(inputs: BoundInputs): Check[] {
  BoundInputs.requireVerified(inputs);
  if (
    inputs.target.repository !== "headironc/designs" ||
    inputs.target.repositoryId !== "1434658318" ||
    inputs.target.target !== "main"
  )
    throw new Error("Inputs do not identify the designs main policy");
  const { policy, candidate } = inputs.paths;
  const documents = inputs.source.entries
    .filter((entry) => entry.path.endsWith(".md"))
    .map((entry) => entry.path);
  for (const required of [
    "AGENTS.md",
    "README.md",
    "ci-architecture.md",
    "runner/README.md",
  ])
    if (!documents.includes(required))
      throw new Error(`Missing required designs document: ${required}`);
  const files = documents.map((file) => path.join(candidate, file));
  const node = tool(inputs, "node", "v26.8.1"),
    prose = tool(inputs, "prose", "3.0.0"),
    ids = tool(inputs, "ids", "1.0.0"),
    lychee = tool(inputs, "lychee", "lychee 0.24.2");
  const proseFile = prose.files?.[0]?.path,
    idsFile = ids.files?.[0]?.path;
  if (
    !proseFile ||
    !idsFile ||
    prose.executable !== node.executable ||
    ids.executable !== node.executable
  )
    throw new Error(
      "Document tools require their fixed scripts and Node runtime",
    );
  const request = (name: string, argv: Check["argv"]): Check => ({
    name,
    argv,
    cwd: policy,
    env: {},
    timeoutMs: 180_000,
  });
  return [
    request("docs.prose", [
      node.executable,
      proseFile,
      "--errors-only",
      ...files,
    ]),
    request("docs.ids", [node.executable, idsFile, ...files]),
    request("docs.links", [
      lychee.executable,
      "--config",
      "/dev/null",
      "--offline",
      "--no-progress",
      "--include-fragments=anchor-only",
      ...files,
    ]),
  ];
}

export function verifyDesigns(
  execution: VerifiedExecution,
  inputs: BoundInputs,
): void {
  verifyChecks(execution, inputs, "docs", designsChecks(inputs));
}
