# Repository instructions

This repository owns cross-repository architecture and the Go runner source.
Agents, Nucleus and designs each own their native CI and business checks.
The legacy shared CI executor and fixed document policy are retired.

- Read `ci-architecture.md` for the current CI direction. Do not restore the
  retired shared executor, frozen policy or admission controller.
- Check changed documents with the pinned formatter using explicit file paths.
- Use `bun run format:check` for the full source check. After changing formatting
  tools, run `bun run typecheck` and `bun run test:format`; see README.md for
  dependency preparation, supported files, explicit repair and recovery.
- Read `runner/README.md` before changing runner code. Run its affected Go checks.
- Run `bun run go:check` for complete runner acceptance; `go:prepare` is a
  separate online preparation step. Run `bun run test:go-tools` after changing
  that entrypoint. README.md owns its environment and recovery instructions.
- Use `bun run links:prepare` for explicit online setup of the pinned link
  checker, then `bun run links:check` for all Markdown internal links. After
  changing this tooling, run `bun run typecheck` and `bun run test:links`.
  README.md owns the source scope, platform limits and recovery instructions.
- Use `bun run go:vuln:prepare` for explicit online preparation, then
  `bun run go:vuln:check` for the selected Linux Go source graph. Run
  `bun run test:go-vuln` after changing this tooling and `test:go-tools` after
  changing its shared command lifecycle. README.md owns findings, scope and recovery.
  `go-vuln-lxd.md` owns the five source-not-affected judgments and their invalidation
  rules; preserve native findings when applying them.
- Run nft integration tests only in the documented private network namespace.
  Do not run the runner binary, install units, or load rules on the real host
  merely to validate a source change.
- Keep runtime configuration, credentials, databases and operation receipts out
  of Git. Preserve historical operation identities and unknown outcomes.
- Deliver one independently verifiable business result per implementation PR.
  Source publication and merge do not themselves perform a deployment switch.
