# Repository instructions

This repository owns the cross-repository architecture, the shared CI executor
in `ci/`, its own document policy in `policy/`, and the Go runner source.
Agents, Nucleus and designs each own their business checks. Kiln owns task images.

- Read `ci/README.md` before changing shared CI tools or the document policy.
  Run the affected checks there; keep local acceptance inside its disposable container.
- Read `runner/README.md` before changing runner code. Run its affected Go checks.
- Run nft integration tests only in the documented private network namespace.
  Do not run the runner binary, install units, or load rules on the real host
  merely to validate a source change.
- Keep runtime configuration, credentials, databases and operation receipts out
  of Git. Preserve historical operation identities and unknown outcomes.
- Deliver one independently verifiable business result per implementation PR.
  Source publication and merge do not themselves perform a deployment switch.
