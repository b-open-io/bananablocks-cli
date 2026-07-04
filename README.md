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
go install github.com/b-open-io/bananablocks-cli@latest
# installs as "bananablocks-cli"; alias or rename it to bb if you prefer
```

Or build from source, which names the binary `bb`:

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

`--verify` recomputes merkle roots **locally** and checks them against block
headers fetched from the server:

```sh
bb tx <txid> --proof               # TSC merkle proof
bb tx <txid> --proof --verify      # verify the TSC path against the header
bb tx <txid> --beef -o tx.beef     # BEEF (BRC-62) to a file
bb tx <txid> --verify              # fetch BEEF, verify every BUMP in it
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

The funding key is a WIF private key supplied via `--wif`, `--wif-file`, or
the `BB_WIF` environment variable:

```sh
BB_API_KEY=bb_live_... BB_WIF=Kx... bb key upgrade --tier pro --yes
```

You'll be shown the price, payee, and funding address and asked to confirm
before anything is signed (skip with `--yes`).

## Development

```sh
make build    # plain Go build, no codegen
make test     # unit tests (SPV vectors, x402 wire contract, payment builder)
make lint     # golangci-lint
```

## License

MIT
