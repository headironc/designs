import { createHash } from "node:crypto";
import {
  closeSync,
  fstatSync,
  ftruncateSync,
  readFileSync,
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
  return files.filter((file) => parsers[path.extname(file)]);
}

export async function formatFiles(
  root: string,
  files: string[],
  write: boolean,
): Promise<string[]> {
  const selected = formatPaths(files);
  if (!selected.length) throw new Error("No supported source files selected");
  const sourceRoot = openSourceRoot(root);
  try {
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
          const formatted = await prettier.format(original, {
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
