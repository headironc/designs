import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  accessSync,
  closeSync,
  constants,
  fstatSync,
  ftruncateSync,
  readFileSync,
  realpathSync,
  writeSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import * as prettier from "prettier";
import {
  listSourcePaths,
  openSourceRoot,
  openSourceFile,
  readSourceEntries,
} from "./source.ts";

const repository = path.resolve(import.meta.dirname, "..");
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

function executable(name: string): string {
  for (const directory of (process.env.PATH ?? "").split(path.delimiter)) {
    if (!directory) continue;
    const file = path.resolve(directory, name);
    try {
      accessSync(file, constants.X_OK);
      return realpathSync(file);
    } catch (error) {
      if (
        !["ENOENT", "EACCES", "ENOTDIR"].includes(
          (error as NodeJS.ErrnoException).code ?? "",
        )
      )
        throw error;
    }
  }
  throw new Error(
    `Missing ${name}; install the Go version in runner/go.mod or set DESIGNS_GO_BIN`,
  );
}

export function verifyGoFormat(goBin = process.env.DESIGNS_GO_BIN): string {
  const gofmt = goBin
    ? realpathSync(path.join(goBin, "gofmt"))
    : executable("gofmt");
  const go = goBin ? path.join(goBin, "go") : executable("go");
  const root = openSourceRoot(repository);
  let required: string | undefined;
  try {
    const descriptor = openSourceFile(root, "runner/go.mod", false);
    try {
      required = /^go (\S+)$/m.exec(readFileSync(descriptor, "utf8"))?.[1];
    } finally {
      closeSync(descriptor);
    }
  } finally {
    closeSync(root);
  }
  if (!required) throw new Error("Missing Go version in runner/go.mod");
  const version = execFileSync(go, ["version", gofmt], {
    encoding: "utf8",
    timeout: 10_000,
    env: {
      PATH: "/usr/bin:/bin",
      // Inspecting these binaries does not require a relocated full SDK.
      GOROOT: path.dirname(gofmt),
      GOTOOLCHAIN: "local",
      GOWORK: "off",
      GOENV: "off",
    },
  }).trim();
  if (version !== `${gofmt}: go${required}`)
    throw new Error(`Formatting requires gofmt built with Go ${required}`);
  return gofmt;
}

export async function formatFiles(
  root: string,
  files: string[],
  write: boolean,
  goBin?: string,
): Promise<string[]> {
  const selected = formatPaths(files);
  if (!selected.length) throw new Error("No supported source files selected");
  const sourceRoot = openSourceRoot(root);
  try {
    const gofmt = selected.some((file) => file.endsWith(".go"))
      ? verifyGoFormat(goBin)
      : undefined;
    // Only the repository root owns format settings; no parent config or EditorConfig lookup.
    const config = openSourceFile(sourceRoot, ".prettierrc.json", false);
    let options: prettier.Options;
    try {
      options = JSON.parse(
        new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(
          readFileSync(config),
        ),
      );
    } finally {
      closeSync(config);
    }
    if (!options || typeof options !== "object" || Array.isArray(options))
      throw new Error("Expected an object in .prettierrc.json");
    const entries = readSourceEntries(sourceRoot, selected);
    const differences: string[] = [];
    for (const entry of entries) {
      const file = entry.path;
      try {
        const target = path.join(root, file);
        const descriptor = openSourceFile(sourceRoot, file, write);
        try {
          const opened = fstatSync(descriptor, { bigint: true });
          if (
            !opened.isFile() ||
            opened.ino !== entry.ino ||
            opened.dev !== entry.dev ||
            (write && opened.nlink !== 1n)
          )
            throw new Error(`Source changed or is linked: ${file}`);
          const bytes = readFileSync(descriptor);
          if (createHash("sha256").update(bytes).digest("hex") !== entry.sha256)
            throw new Error(`Source changed before formatting: ${file}`);
          const original = new TextDecoder("utf-8", {
            fatal: true,
            ignoreBOM: true,
          }).decode(bytes);
          const formatted = file.endsWith(".go")
            ? execFileSync(gofmt!, [], {
                input: original,
                encoding: "utf8",
                timeout: 30_000,
                maxBuffer: 16 * 1024 * 1024,
                env: { PATH: "/usr/bin:/bin" },
                stdio: ["pipe", "pipe", "pipe"],
              })
            : await prettier.format(original, {
                ...options,
                parser: parsers[path.extname(file)]!,
                filepath: target,
              });
          if (original !== formatted) {
            differences.push(file);
            if (write) {
              let named;
              try {
                const currentRoot = openSourceRoot(root);
                try {
                  const originalRoot = fstatSync(sourceRoot, { bigint: true });
                  const namedRoot = fstatSync(currentRoot, { bigint: true });
                  if (
                    originalRoot.ino !== namedRoot.ino ||
                    originalRoot.dev !== namedRoot.dev
                  )
                    throw new Error("Source root changed");
                  const currentFile = openSourceFile(currentRoot, file, false);
                  try {
                    named = fstatSync(currentFile, { bigint: true });
                  } finally {
                    closeSync(currentFile);
                  }
                } finally {
                  closeSync(currentRoot);
                }
              } catch (error) {
                throw new Error(`Source changed during formatting: ${file}`, {
                  cause: error,
                });
              }
              const current = fstatSync(descriptor, { bigint: true });
              if (
                named.ino !== opened.ino ||
                named.dev !== opened.dev ||
                current.nlink !== 1n ||
                current.size !== opened.size ||
                current.mtimeNs !== opened.mtimeNs ||
                current.ctimeNs !== opened.ctimeNs
              )
                throw new Error(`Source changed during formatting: ${file}`);
              // Write only through the descriptor held across async formatting.
              const output = Buffer.from(formatted);
              let offset = 0;
              while (offset < output.length) {
                const written = writeSync(
                  descriptor,
                  output,
                  offset,
                  output.length - offset,
                  offset,
                );
                if (!written)
                  throw new Error(`Incomplete format write: ${file}`);
                offset += written;
              }
              ftruncateSync(descriptor, output.length);
            }
          }
        } finally {
          closeSync(descriptor);
        }
      } catch (error) {
        throw new Error(
          `${file}: ${error instanceof Error ? error.message : String(error)}`,
          { cause: error },
        );
      }
    }
    return differences;
  } finally {
    closeSync(sourceRoot);
  }
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const [mode, ...operands] = process.argv.slice(2);
    if (!["--check", "--write"].includes(mode!))
      throw new Error(
        "Usage: node scripts/format.ts --check|--write [FILE...]",
      );
    const files = operands.length ? operands : listSourcePaths(repository);
    const differences = await formatFiles(
      repository,
      files,
      mode === "--write",
    );
    for (const file of differences)
      console.log(
        `${mode === "--write" ? "Formatted" : "Unformatted"}: ${file}`,
      );
    console.log(`Checked ${formatPaths(files).length} source files`);
    if (differences.length && mode === "--check") process.exitCode = 1;
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 2;
  }
}
