# Repository instructions

This repository owns cross-repository architecture and the Go runner source.
Agents, Nucleus and designs each own their native CI and business checks.
The legacy shared CI executor and fixed document policy are retired.

- Read `ci-architecture.md` for the current CI direction. Do not restore the
  retired shared executor, frozen policy or admission controller.
- Check changed documents with the pinned formatter using explicit file paths.
- Read `runner/README.md` before changing runner code. Run its affected Go checks.
- Run nft integration tests only in the documented private network namespace.
  Do not run the runner binary, install units, or load rules on the real host
  merely to validate a source change.
- Keep runtime configuration, credentials, databases and operation receipts out
  of Git. Preserve historical operation identities and unknown outcomes.
- Deliver one independently verifiable business result per implementation PR.
  Source publication and merge do not themselves perform a deployment switch.
