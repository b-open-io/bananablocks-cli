# bb — BananaBlocks CLI

`bb` is a command-line client for the [BananaBlocks](https://bananablocks.com)
BSV explorer API: block/transaction/address/token queries, local SPV proof
verification (BEEF and TSC), transaction broadcast, live event streaming over
WebSocket, and **x402 pay-to-upgrade** — buying API rate-limit tiers with an
on-chain BSV payment, straight from the terminal.

Because BananaBlocks also exposes a WhatsonChain-compatible surface, most
commands work against any compatible host via `--host`.

## Install

```sh
go install github.com/b-open-io/bananablocks-cli/cmd/bb@latest   # installs as "bb"
```

Or build from source:

```sh
make build     # → bin/bb
make install   # → $GOBIN, versioned via ldflags
```

## Configuration

| Flag        | Env          | Default                    |
|-------------|--------------|----------------------------|
| `--host`    | `BB_HOST`    | `https://bananablocks.com` |
| `--api-key` | `BB_API_KEY` | (anonymous)                |
| `--timeout` |              | `30s`                      |
| `--chain`   |              | `main`                     |

## Commands

### Query

```sh
bb block tip                       # chain tip
bb block 800000                    # block by height or hash
bb block 800000 --protocols        # protocol counts in the block
bb blocks --limit 10               # recent blocks
bb tx <txid>                       # transaction JSON
bb tx <txid> --hex                 # raw hex
bb address <addr>                  # address summary
bb address <addr> --utxos          # unspent outputs
bb address <addr> --balance        # balance only
bb address <addr> --tokens         # BSV-21 token balances
bb tokens --limit 25               # token list
bb token <tokenId> --holders       # token holders
bb stats network                   # network stats (also: summary, richlist,
                                   # mempool, protocols, price, rates)
bb get "blocks?limit=5"            # any API path, pretty-printed
```

### SPV verification

`--verify` recomputes merkle roots **locally** and, by default, confirms them
against an **independent block-headers service** (a proof-of-work-validated
chain, chosen per `--chain`; override with `--headers-url`). This means a
matching proof genuinely belongs to the honest chain — a malicious indexer
can't pass a fabricated proof by also serving a matching header. Pass
`--no-headers` to fall back to cross-checking against the serving API's own
blocks, which is weaker (it re-trusts that server) and clearly labelled as
such in the output.

```sh
bb tx <txid> --proof               # TSC merkle proof
bb tx <txid> --proof --verify      # verify the TSC path against an independent header
bb tx <txid> --beef -o tx.beef     # BEEF (BRC-62) to a file
bb tx <txid> --verify              # fetch BEEF, verify every BUMP in it
bb tx <txid> --verify --no-headers # weaker: cross-check against the serving API only
```

### Broadcast

```sh
bb broadcast 01000000...           # hex string
bb broadcast tx.hex                # file (hex or binary)
cat tx.hex | bb broadcast -        # stdin
bb broadcast tx1.hex tx2.hex       # batch via the multi endpoint
```

### Live events

```sh
bb watch                           # new blocks (default channel)
bb watch blocks mempool            # multiple channels
bb watch address:<addr>            # txs touching an address
```

One JSON event per line; reconnects automatically.

### On-chain content

```sh
bb media <txid>:<vout>                       # unified media endpoint
bb media <txid>:<vout> --kind inscription    # or nft, bfile
bb media <txid>:<vout> --stdout | jq .       # stream to stdout
```

### API key & x402 pay-to-upgrade

```sh
bb key usage                       # tier, rate limit, today's usage
bb key upgrade --dry-run           # fetch the 402 challenge, don't pay
bb key upgrade --tier pro          # buy a tier upgrade on-chain
bb key upgrade                     # resume a saved pending payment (never pays twice)
```

`bb key upgrade` implements the x402 (BRC-120-style, scheme `bsv-tx-v1`)
payment flow:

1. The server answers with a **402 challenge**: a price in satoshis and a
   per-challenge P2PKH payment address, expiring after a short TTL.
2. `bb` builds and signs a transaction paying that address from your funding
   key's UTXOs (change returns to your address).
3. The signed transaction is submitted back as an `X402-Proof` header. **The
   server broadcasts it** — `bb` never does, so nothing leaves your wallet if
   the upgrade is rejected.

The funding key is a WIF private key. Prefer `--wif-file`, which keeps the key
out of shell history and process environments:

```sh
printf '%s\n' '<funding-wif>' > ~/.config/bb/funding.wif
chmod 600 ~/.config/bb/funding.wif
BB_API_KEY=bb_live_... bb key upgrade --tier pro --wif-file ~/.config/bb/funding.wif --yes
```

`--wif` and the `BB_WIF` environment variable also work for ephemeral or test
use, but are less safe: shells, inherited environments, logs, and scrollback
can expose the value. When more than one source is set, `--wif-file` wins,
then `--wif`, then `BB_WIF`.

You'll be shown the price, payee, and funding address and asked to confirm
before anything is signed (skip with `--yes`).

#### Pending payments and resuming

The server can answer the proof with **202 Accepted** and a `Retry-After`
header: it broadcast your payment, but the network has not accepted it yet.
A 202 grants nothing, and the challenge stays open. `bb` waits `Retry-After`
seconds (10 if the header is missing or unreadable, clamped to 1–60) and
resubmits the **same** proof until the server answers 200, printing one line
per pending answer to stderr:

```
payment 3f2a… not settled yet (payment broadcast but not yet accepted by the network; resubmit the same proof); resubmitting the same proof in 10s
```

`bb` treats a 429, a 5xx (a node draining or restarting behind the load
balancer, say) and a submit that fails in transit (the connection drops or
`--timeout` fires) the same way: it waits (`Retry-After` when sent, else 10
seconds) and resubmits the same proof.

`--wait` (default `10m`) caps how long `bb` keeps doing that. It is separate
from `--timeout`, which bounds each request. Ctrl-C stops the wait at once.

Before the first submit, `bb` saves the proof to
`<user config dir>/bb/pending-upgrades.json` (mode 0600; `~/.config/bb/` on
Linux, `~/Library/Application Support/bb/` on macOS), keyed by a fingerprint of
the host and API key (the key itself is not written; the host's case, a
default port, a trailing slash and whitespace around the key do not change it)
and the challenge id.
**If the wait runs out, the run is interrupted, or the connection drops, your
payment is saved: rerun `bb key upgrade` and it resubmits the saved proof
instead of building a new payment. Do not pay again.** Resuming works even
after the challenge's `expires_at`, when the server has already moved on to a
new challenge id; a second payment for the same challenge is not credited.
`bb` also resumes, rather than pays, whenever the server hands out a challenge
id that already has a saved proof, even one saved under another spelling of
the host or key.
While a saved proof is unsettled, the rate-limit offer below does not pay
either; it points you at `bb key upgrade`.

`bb` removes the saved entry when the upgrade settles, and when the server
says the proof can never settle (422 rejected by broadcast, 409 payment txid
already used, 410 expired with no payment the network holds, 404 `challenge
not found`, 400/402). Any other 404, such as a bare `404 page not found` from
a server where the upgrade route is not enabled, keeps the entry. If the server
says **challenge already consumed**, an earlier submit most likely settled and
its response was lost: `bb` reads the key's tier from `/api/v1/key/usage` and
reports success when the key is at or above the purchased tier and the proof
was last submitted less than an hour earlier. Below that tier within the hour,
it keeps the entry and asks you to rerun, since a new tier can take a minute to
show on every server.
A submit answered **challenge already consumed** settled nothing, so it does
not count as a submit and rerunning does not restart that hour.
An entry last submitted longer ago can never settle again, so `bb` removes it
and exits non-zero whatever the tier: below it (say the tier has since lapsed)
the payment is gone, and at or above it the payment settled long ago, so it is
not reported as this run's upgrade. Either way this run buys nothing, and the
next `bb key upgrade` buys a fresh upgrade or renewal.

`bb` also advertises `X-Payment-Accept: x402` on every API request, so when
a keyed command gets **rate-limited** the server answers with a payable 402
challenge instead of a bare 429. `bb` prints the upgrade terms and either
points you at `bb key upgrade` or — when running interactively with a
funding key already available — offers to pay the challenge on the spot:

```
Error: HTTP 402: rate limit exceeded
This API key can be upgraded on-chain (x402):
  tier:    pro (30 days)
  price:   5000000 sats
  pay to:  1...
  from:    1...
Pay 5000000 sats now to upgrade to "pro"? [y/N]:
```

Prices are set by the server and quoted per challenge — bb always shows the
live price and confirms before signing.

Scripts are unaffected: without a TTY it prints the `bb key upgrade` hint and
exits non-zero as before. The inline offer only pays from a key set explicitly
for that command (`--wif-file` or `--wif`); an inherited `BB_WIF` prints the
hint instead, so an ambient funding key can't be spent from an unrelated
command that happens to hit a rate limit.

## Development

```sh
make build    # plain Go build, no codegen
make test     # unit tests (SPV vectors, x402 wire contract, payment builder)
make lint     # golangci-lint
```

## Security

`bb` handles a funding private key and verifies proofs locally. See
[SECURITY.md](SECURITY.md) for how keys and payments are handled and how to
report a vulnerability privately.

## License

MIT
