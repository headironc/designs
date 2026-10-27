# Repository instructions

This repository owns the cross-repository architecture and the Go runner source.
Consumer repositories own their business workflows; Kiln owns task images.

- Read `runner/README.md` before changing runner code. Run its affected Go checks.
- Run nft integration tests only in the documented private network namespace.
  Do not run the runner binary, install units, or load rules on the real host
  merely to validate a source change.
- Keep runtime configuration, credentials, databases and operation receipts out
  of Git. Preserve historical operation identities and unknown outcomes.
- Deliver one independently verifiable business result per implementation PR.
  Source publication and merge do not themselves perform a deployment switch.
