import { execFileSync } from "node:child_process";
import {
  readFileSync,
  readdirSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import * as prettier from "prettier";
import { listSourcePaths, readSourceEntries } from "../ci/source.ts";

const parsers: Record<string, string> = {
  ".ts": "typescript",
  ".tsx": "typescript",
  ".js": "babel",
  ".jsx": "babel",
  ".mjs": "babel",
  ".cjs": "babel",
  ".json": "json",
  ".md": "markdown",
  ".yml": "yaml",
  ".yaml": "yaml",
};

export function formatPaths(files: string[]): string[] {
  return files.filter(
    (file) => file.endsWith(".go") || parsers[path.extname(file)],
  );
}

export function prettierFiles(policy: string): string[] {
  const root = path.join(policy, "node_modules/prettier");
  return readdirSync(root, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => path.join(entry.parentPath, entry.name))
    .sort();
}

export function verifyGoFormat(goBin: string): string {
  const gofmt = path.join(realpathSync(goBin), "gofmt");
  const version = execFileSync(path.join(goBin, "go"), ["version", gofmt], {
    encoding: "utf8",
    timeout: 10_000,
    // Version inspection needs only the two binaries, not a relocated full SDK.
    env: {
      PATH: "/usr/bin:/bin",
      GOROOT: path.dirname(gofmt),
      GOTOOLCHAIN: "local",
      GOWORK: "off",
      GOENV: "off",
    },
  }).trim();
  if (version !== `${gofmt}: go1.27.3`)
    throw new Error("Formatting requires gofmt built with Go 1.27.3");
  return gofmt;
}

/** Fixed policy options; never resolve candidate config, ignores or plugins. */
export async function formatFiles(
  root: string,
  files: string[],
  gofmt: string,
  write: boolean,
): Promise<string[]> {
  if (prettier.version !== "3.9.9")
    throw new Error("Formatting requires Prettier 3.9.9");
  const selected = formatPaths(files);
  if (!selected.length) throw new Error("No supported source files selected");
  // Reuse source validation so links and paths outside the source cannot be rewritten.
  readSourceEntries(root, selected);
  const differences: string[] = [];
  for (const file of selected) {
    const target = path.join(root, file);
    const original = new TextDecoder("utf-8", {
      fatal: true,
      ignoreBOM: true,
    }).decode(readFileSync(target));
    const formatted = file.endsWith(".go")
      ? execFileSync(gofmt, [], {
          input: original,
          encoding: "utf8",
          timeout: 30_000,
          maxBuffer: 16 * 1024 * 1024,
          env: { PATH: "/usr/bin:/bin" },
        })
      : await prettier.format(original, {
          parser: parsers[path.extname(file)]!,
          proseWrap: "preserve",
          embeddedLanguageFormatting: "off",
          endOfLine: "lf",
        });
    if (original !== formatted) {
      differences.push(file);
      if (write) writeFileSync(target, formatted);
    }
  }
  return differences;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    if (process.argv.length === 4 && process.argv[2] === "--gofmt-version") {
      verifyGoFormat(process.argv[3]!);
      console.log("go1.27.3");
    } else {
      const [mode, rootFlag, rootArg, goFlag, goBin, ...operands] =
        process.argv.slice(2);
      if (
        !["--check", "--write"].includes(mode!) ||
        rootFlag !== "--root" ||
        !rootArg ||
        goFlag !== "--go-bin" ||
        !goBin
      )
        throw new Error(
          "Usage: node policy/format.ts --check|--write --root DIRECTORY --go-bin DIRECTORY [FILE...]",
        );
      const root = realpathSync(rootArg);
      const gofmt = verifyGoFormat(goBin);
      const files = operands.length ? operands : listSourcePaths(root);
      const differences = await formatFiles(
        root,
        files,
        gofmt,
        mode === "--write",
      );
      for (const file of differences)
        console.log(
          `${mode === "--write" ? "Formatted" : "Unformatted"}: ${file}`,
        );
      console.log(`Checked ${formatPaths(files).length} source files`);
      if (differences.length && mode === "--check") process.exitCode = 1;
    }
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 2;
  }
}
