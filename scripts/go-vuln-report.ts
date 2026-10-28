import { createHash } from "node:crypto";

export const lxdModule = "github.com/canonical/lxd";
export const lxdIDs = [
  "GO-2025-3999",
  "GO-2025-4003",
  "GO-2025-4121",
  "GO-2026-4595",
  "GO-2026-5306",
] as const;
export const lxdPackages = [
  "client",
  "shared",
  "shared/api",
  "shared/cancel",
  "shared/ioprogress",
  "shared/logger",
  "shared/osarch",
  "shared/revert",
  "shared/simplestreams",
  "shared/tcp",
  "shared/termios",
  "shared/units",
  "shared/version",
  "shared/ws",
]
  .map((name) => `${lxdModule}/${name}`)
  .sort();
export const hashJSON = (value: unknown) =>
  createHash("sha256").update(JSON.stringify(value)).digest("hex");
const object = (value: unknown): Record<string, unknown> => {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("Expected report object");
  return value as Record<string, unknown>;
};
const string = (value: unknown): string => {
  if (typeof value !== "string" || !value)
    throw new Error("Missing report string");
  return value;
};
const array = (value: unknown): unknown[] => {
  if (!Array.isArray(value)) throw new Error("Expected report array");
  return value;
};
const strings = (value: unknown) => array(value).map(string);
const same = (a: unknown, b: unknown) =>
  JSON.stringify(a) === JSON.stringify(b);
const sorted = (values: string[]) => [...new Set(values)].sort();

export interface ScanIdentity {
  version: string;
  goVersion: string;
  hash: string;
  database: string;
}
export interface Frame {
  module: string;
  version?: string;
  package?: string;
  function?: string;
}
export interface Finding {
  osv: string;
  trace: Frame[];
}
export interface ScanReport {
  config: Record<string, unknown>;
  sbom: Record<string, unknown>;
  osvs: Map<string, Record<string, unknown>>;
  findings: Finding[];
  reachable: Finding[];
}
export function verifyNativeStatus(report: ScanReport, code: 0 | 3): boolean {
  const findings = code === 3;
  if (findings !== Boolean(report.reachable.length))
    throw new Error("Native text status disagrees with symbol findings");
  return findings;
}

export function parseScan(
  messages: unknown,
  identity: ScanIdentity,
): ScanReport {
  let config: Record<string, unknown> | undefined;
  let sbom: Record<string, unknown> | undefined;
  const osvs = new Map<string, Record<string, unknown>>();
  const findings: Finding[] = [];
  for (const [index, entry] of array(messages).entries()) {
    const event = object(entry),
      keys = Object.keys(event);
    if (keys.length !== 1) throw new Error("Expected one scan event");
    const key = keys[0]!,
      value = object(event[key]);
    if (index === 0 && key !== "config")
      throw new Error("Missing initial scan config");
    switch (key) {
      case "config":
        if (config || index !== 0) throw new Error("Duplicate scan config");
        config = value;
        for (const [name, expected] of Object.entries({
          protocol_version: "v1.0.0",
          scanner_name: "govulncheck",
          scanner_version: identity.version,
          go_version: `go${identity.goVersion}`,
          scan_level: "symbol",
          scan_mode: "source",
          db: identity.database,
        }))
          if (value[name] !== expected)
            throw new Error(`Unexpected scan config ${name}`);
        if (
          Object.keys(value).some(
            (name) =>
              ![
                "protocol_version",
                "scanner_name",
                "scanner_version",
                "go_version",
                "scan_level",
                "scan_mode",
                "db",
                "db_last_modified",
              ].includes(name),
          )
        )
          throw new Error("Unknown scan config field");
        break;
      case "SBOM":
        if (sbom) throw new Error("Duplicate scan SBOM");
        sbom = value;
        if (
          value.go_version !== `go${identity.goVersion}` ||
          !strings(value.roots).length ||
          !array(value.modules).length
        )
          throw new Error("Invalid scan SBOM");
        for (const module of array(value.modules)) {
          const m = object(module);
          string(m.path);
          if (m.version !== undefined) string(m.version);
        }
        break;
      case "osv": {
        const id = string(value.id);
        // One advisory can be emitted for several modules. Preserve identical
        // repeats, but never silently replace conflicting database facts.
        if (osvs.has(id) && !same(osvs.get(id), value))
          throw new Error(`Conflicting OSV ${id}`);
        string(value.modified);
        array(value.affected);
        osvs.set(id, value);
        break;
      }
      case "finding": {
        const osv = string(value.osv);
        const trace = array(value.trace).map((entry) => {
          const frame = object(entry);
          string(frame.module);
          for (const name of ["version", "package", "function", "receiver"])
            if (frame[name] !== undefined) string(frame[name]);
          if (frame.function && !frame.package)
            throw new Error("Function without package");
          return frame as unknown as Frame;
        });
        if (!trace.length) throw new Error("Empty finding trace");
        findings.push({ osv, trace });
        break;
      }
      case "progress":
        string(value.message);
        break;
      default:
        throw new Error(`Unknown scan event ${key}`);
    }
  }
  if (!config || !sbom) throw new Error("Incomplete scan config/SBOM");
  for (const finding of findings)
    if (!osvs.has(finding.osv))
      throw new Error(`Finding without OSV ${finding.osv}`);
  return {
    config,
    sbom,
    osvs,
    findings,
    reachable: findings.filter((f) => Boolean(f.trace[0]!.function)),
  };
}

export interface PackageGraph {
  roots: string[];
  lxd: string[];
  unsupported: string[];
}
export function inspectPackages(values: unknown, module: string): PackageGraph {
  const roots: string[] = [],
    lxd: string[] = [],
    unsupported: string[] = [];
  for (const value of array(values)) {
    const p = object(value),
      name = string(p.ImportPath);
    if (p.Error || p.DepsErrors || p.Incomplete)
      throw new Error(`Incomplete Go package ${name}`);
    const m = p.Module === undefined ? undefined : object(p.Module);
    if (p.DepOnly !== true) roots.push(name.replace(/ \[.*\]$/, ""));
    if (m?.Path === lxdModule) lxd.push(name);
    if (m?.Path === module) {
      if (m.Replace) throw new Error("Unexpected main module replacement");
      for (const field of [
        "CgoFiles",
        "CFiles",
        "CXXFiles",
        "MFiles",
        "HFiles",
        "FFiles",
        "SFiles",
        "SwigFiles",
        "SwigCXXFiles",
        "SysoFiles",
        "IgnoredOtherFiles",
        "EmbedFiles",
        "EmbedPatterns",
        "TestEmbedFiles",
        "TestEmbedPatterns",
        "XTestEmbedFiles",
        "XTestEmbedPatterns",
      ])
        if (p[field] !== undefined && array(p[field]).length)
          unsupported.push(`${name}: ${field}`);
    }
  }
  if (!roots.length) throw new Error("Missing Go root packages");
  return {
    roots: sorted(roots),
    lxd: sorted(lxd),
    unsupported: sorted(unsupported),
  };
}

export const runnerInputHash = (entries: { path: string; sha256: string }[]) =>
  hashJSON(
    entries
      .filter(
        (entry) =>
          entry.path.startsWith("runner/") &&
          (entry.path.endsWith(".go") ||
            ["runner/go.mod", "runner/go.sum"].includes(entry.path)),
      )
      .map(({ path, sha256 }) => ({ path, sha256 }))
      .sort((a, b) => Buffer.compare(Buffer.from(a.path), Buffer.from(b.path))),
  );
export const scopeIdentity = (identity: ScanIdentity) => ({
  scanner: identity.version,
  scannerSHA256: identity.hash,
  go: identity.goVersion,
  goos: "linux",
  goarch: "amd64",
  goamd64: "v1",
  cgo: "1",
  mode: "source",
  scan: "symbol",
  test: true,
  tags: ["integration"],
  database: identity.database,
});

export function evaluateLXD(
  report: ScanReport,
  recordValue: unknown,
  actual: {
    identity: ScanIdentity;
    sourceSHA256: string;
    goSum: string;
    graph: PackageGraph;
  },
) {
  const record = object(recordValue);
  if (
    !same(
      Object.keys(record).sort(),
      [
        "schema",
        "reviewedAt",
        "sourceSHA256",
        "scope",
        "module",
        "packages",
        "advisories",
      ].sort(),
    ) ||
    record.schema !== 1
  )
    throw new Error("Invalid LXD disposition record");
  if (!Number.isFinite(Date.parse(string(record.reviewedAt))))
    throw new Error("Invalid reviewedAt provenance");
  for (const hash of [record.sourceSHA256])
    if (!/^[a-f0-9]{64}$/.test(string(hash)))
      throw new Error("Invalid disposition hash");
  const module = object(record.module);
  if (
    !same(
      Object.keys(module).sort(),
      ["path", "version", "sum", "goModSum"].sort(),
    ) ||
    module.path !== lxdModule
  )
    throw new Error("Invalid disposition module");
  for (const key of ["version", "sum", "goModSum"]) string(module[key]);
  const packages = strings(record.packages);
  if (!same(packages, lxdPackages))
    throw new Error("Disposition must name the exact 14 LXD packages");
  const advisories = array(record.advisories).map(object);
  if (!same(advisories.map((a) => a.id).sort(), lxdIDs))
    throw new Error("Disposition must name exactly the five LXD IDs");
  for (const a of advisories) {
    if (
      !same(Object.keys(a).sort(), ["id", "sha256", "reason"].sort()) ||
      !/^[a-f0-9]{64}$/.test(string(a.sha256))
    )
      throw new Error("Invalid advisory disposition");
    string(a.reason);
  }
  const reasons: string[] = [];
  if (record.sourceSHA256 !== actual.sourceSHA256)
    reasons.push("Runner Go source or module locks changed");
  if (!same(record.scope, scopeIdentity(actual.identity)))
    reasons.push("Scanner or build scope changed");
  if (!same(packages, actual.graph.lxd))
    reasons.push("LXD package set changed");
  if (actual.graph.unsupported.length)
    reasons.push(
      `Unreviewed non-Go or embedded inputs: ${actual.graph.unsupported.join(", ")}`,
    );
  const sumLines = new Set(actual.goSum.split("\n"));
  if (
    !sumLines.has(`${lxdModule} ${module.version} ${module.sum}`) ||
    !sumLines.has(`${lxdModule} ${module.version}/go.mod ${module.goModSum}`)
  )
    reasons.push("LXD version or checksum changed");
  const selected = array(report.sbom.modules)
    .map(object)
    .filter((m) => m.path === lxdModule);
  if (selected.length !== 1 || selected[0]!.version !== module.version)
    reasons.push("LXD selected module changed");
  const disposed: string[] = [];
  const pending: string[] = [];
  for (const a of advisories)
    if (a.sha256 !== hashJSON(report.osvs.get(a.id as string) ?? null))
      reasons.push(`OSV content changed or missing: ${a.id}`);
  for (const id of sorted(report.reachable.map((f) => f.osv))) {
    const a = advisories.find((a) => a.id === id);
    const matches = report.reachable
      .filter((f) => f.osv === id)
      .every(
        (f) =>
          f.trace[0]!.module === lxdModule &&
          f.trace[0]!.version === module.version &&
          packages.includes(f.trace[0]!.package!),
      );
    if (!a || !matches) pending.push(id);
    else if (!reasons.length && a.sha256 === hashJSON(report.osvs.get(id)))
      disposed.push(id);
    else pending.push(id);
  }
  // A binding failure invalidates this one reviewed set, including earlier IDs.
  if (reasons.length)
    return {
      disposed: [],
      pending: sorted(report.reachable.map((f) => f.osv)),
      reasons,
    };
  return { disposed, pending, reasons };
}
