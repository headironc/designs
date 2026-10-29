# Repository instructions

This repository owns cross-repository architecture and runner recovery navigation.
Agents, Nucleus and designs each own their native CI and business checks.
The legacy shared CI executor and fixed document policy are retired.

- Read `ci-architecture.md` for the current CI direction. Do not restore the
  retired shared executor, frozen policy or admission controller.
- Check changed documents with the pinned formatter using explicit file paths.
- Use `bun run format:check` for the full source check. After changing formatting
  tools, run `bun run typecheck` and `bun run test:format`; see README.md for
  dependency preparation, supported files, explicit repair and recovery.
- Use `bun run links:prepare` for explicit online setup of the pinned link
  checker, then `bun run links:check` for all Markdown internal links. After
  changing this tooling, run `bun run typecheck` and `bun run test:links`.
  README.md owns the source scope, platform limits and recovery instructions.
- Read `runner/README.md` before changing runner architecture or recovery guidance.
- Keep runtime configuration, credentials, databases and operation receipts out
  of Git. Preserve historical operation identities and unknown outcomes.
- Deliver one independently verifiable business result per implementation PR.
  Source publication and merge do not themselves perform a deployment switch.
