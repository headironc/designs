import path from "node:path";
import { BoundInputs } from "../.shared/ci/binding.ts";
import type { Check } from "../.shared/ci/contract.ts";
import { VerifiedExecution, verifyChecks } from "../.shared/ci/execution.ts";
import { designsChecks as documentChecks } from "./docs.ts";
import { formatPaths, prettierFiles } from "./format.ts";

/** The complete maintenance policy includes both documents and source formatting. */
export function designsChecks(inputs: BoundInputs): Check[] {
  const checks = documentChecks(inputs);
  const node = inputs.paths.tools.find((tool) => tool.name === "node")!;
  const prettier = inputs.paths.tools.find((tool) => tool.name === "prettier");
  const go = inputs.paths.tools.find((tool) => tool.name === "go-format");
  const gofmt = go?.files?.find((file) => path.basename(file.path) === "gofmt");
  const goBinary = go?.files?.find((file) => path.basename(file.path) === "go");
  // Hosted verification runs from local P but reconstructs the original remote argv.
  const localPolicy = path.resolve(import.meta.dirname, "..");
  const resources = prettierFiles(localPolicy).map((file) =>
    path.join(inputs.paths.policy, path.relative(localPolicy, file)),
  );
  if (
    !prettier ||
    prettier.version !== "3.9.9" ||
    prettier.executable !== node.executable ||
    JSON.stringify(prettier.files?.map((file) => file.path).sort()) !==
      JSON.stringify(resources) ||
    !gofmt ||
    !goBinary ||
    go?.executable !== node.executable ||
    go.version !== "go1.27.3" ||
    path.dirname(goBinary.path) !== path.dirname(gofmt.path)
  )
    throw new Error("Missing pinned designs format tools");
  const files = formatPaths(inputs.source.entries.map((entry) => entry.path));
  if (!files.some((file) => file.endsWith(".go")))
    throw new Error("Missing designs Go sources");
  checks.push({
    name: "format.sources",
    argv: [
      node.executable,
      path.join(inputs.paths.policy, "policy/format.ts"),
      "--check",
      "--root",
      inputs.paths.candidate,
      "--go-bin",
      path.dirname(gofmt.path),
      ...files,
    ],
    cwd: inputs.paths.policy,
    env: {},
    timeoutMs: 180_000,
  });
  return checks;
}

export function verifyDesigns(
  execution: VerifiedExecution,
  inputs: BoundInputs,
): void {
  verifyChecks(execution, inputs, "maintenance", designsChecks(inputs));
}
