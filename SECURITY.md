# Security Policy

## Reporting a vulnerability

Please report security issues privately rather than opening a public issue.

- Use GitHub's **[Report a vulnerability](https://github.com/b-open-io/bananablocks-cli/security/advisories/new)**
  (Security → Advisories) to open a private advisory, or
- email **security@bopen.io** with the details.

Include a description, affected version or commit, and steps to reproduce.
We aim to acknowledge reports within a few business days and will coordinate a
fix and disclosure timeline with you.

Please do not disclose the issue publicly until a fix is available.

## Scope

`bb` is a client. It handles a funding **WIF private key** to build and sign
x402 payment transactions, and it verifies SPV proofs locally. Reports that
bear directly on those responsibilities are the highest priority — for example:

- disclosure of the funding key (logging, error messages, argv/environment
  leakage, temp files);
- signing or broadcasting a transaction that pays a destination or amount other
  than the one the user confirmed;
- an SPV proof (BEEF or TSC) that verifies as valid when it is not, or that is
  accepted for a transaction the user did not request.

## Handling of keys and payments

A few properties are intended by design; a deviation from any of them is a bug
worth reporting:

- **Keys are never logged or printed.** `bb` prints the *derived address*, never
  the WIF. Prefer `--wif-file` (kept out of shell history and inherited
  environments) over `--wif` / `BB_WIF`. `*.wif` files are git-ignored.
- **`bb` never broadcasts.** It builds and signs the payment locally and submits
  it to the server as a proof; the server broadcasts. Nothing leaves your wallet
  if the upgrade is rejected.
- **The confirmed destination is verified.** The payee address shown to you is
  checked against the locking script the transaction actually pays before
  signing (`--yes` skips only the interactive prompt, not this check).
- **Independent proof verification.** `--verify` recomputes merkle roots locally
  and confirms them against an independent block-headers service by default;
  falling back to the serving API is opt-in (`--no-headers`) and clearly
  labelled as weaker.

## Supported versions

This project is pre-1.0. Security fixes are applied to the latest release and
`main`; older tags are not maintained.
