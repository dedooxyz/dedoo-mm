# Dedoo Multisig Manager

A self-hosted keeper daemon and web UI for **2-of-3 multisig wallets** on
WojakCoin mainnet and Junkcoin.

Three independent operators each run one keeper node. A transaction needs two
independent signatures before it can be broadcast; no single node ever holds
all keys.

## Features

- **2-of-3 (or M-of-3) native P2SH multisig** on WojakCoin and Junkcoin.
- **One chain per node**, enforced at startup and when adding wallets.
- **Multiple wallets per node**, switchable from the sidebar. Balances,
  proposals, history, transfers and UTXOs follow the selected wallet.
- **Node-wide signing keys**: add a WIF once and it can sign every wallet whose
  redeem script contains its public key.
- **Watch-only wallets** for tracking and verifying without a private key.
- **Human-in-the-loop by default**: proposals wait for manual approval, and a
  completed transaction waits for an explicit broadcast.
- **P2P coordination** over libp2p with DHT rendezvous, a private relay, and
  optional static peers. No central coordinator.
- **Independent verification** of every proposal before this node signs.
- **Limits and safety rails**: per-transaction input cap (hard maximum 600),
  1 MB transaction size check, coinbase maturity enforcement, proposal ID
  collision protection, and canonical destination checks.
- **Single static binary** with an embedded web UI and Basic Auth.

## Security model

- Each operator generates and holds **only their own WIF**.
- WIFs and imported keys are stored locally (`0600`) and are never returned by
  the API.
- `keeper.env` and `keeper-keys.json` must never be committed or shared.
- A node can start in **manager mode** without a WIF/vault; the wallet and key
  are added from the UI.
- Two signatures from independent operators are required; `AUTO_SIGN` and
  `AUTO_BROADCAST` are off by default.
- See [SECURITY.md](SECURITY.md) and
  [docs/DISTRIBUTION.md](docs/DISTRIBUTION.md).

## Supported chains

| Chain | P2PKH | P2SH | WIF | Coinbase maturity |
|---|---|---|---|---|
| WojakCoin mainnet (`wojakcoin`) | 73 | 5 | 201 | 20 |
| Junkcoin (`junkcoin`) | 16 | 5 | 144 | 70 |

A custom chain can be configured with `CHAIN="custom"` and explicit version
bytes. A node never mixes chains.

## Repository layout

```
cmd/wojak-keeper/        entrypoint
internal/api/            REST API + embedded web UI
internal/chain/          chain profiles, electrs/RPC clients, tx builder
internal/config/         environment configuration and validation
internal/p2p/            libp2p node, protocol v3 messages
internal/processor/      keeper state machine, wallets, keys, proposals
config/                  keeper.env.example
deploy/                  systemd unit and installer
docs/                    deployment, distribution and agent guides
```

## Build

Requires Go 1.24+ (see `go.mod`).

```bash
go build ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o wojak-keeper ./cmd/wojak-keeper
```

## Install (one keeper node)

```bash
VERSION=v0.1.0
tar xzf "dedoo-multisig-manager-${VERSION}-linux-amd64.tar.gz"
cd "dedoo-multisig-manager-${VERSION}-linux-amd64"
./install.sh ./wojak-keeper .
$EDITOR /opt/wojak-keeper/keeper.env
systemctl enable --now wojak-keeper
```

The UI/API listens on `127.0.0.1:8080` by default. Use an SSH tunnel, or bind
to a trusted LAN address with Basic Auth enabled.

## Deploy a trio

1. Read [docs/DISTRIBUTION.md](docs/DISTRIBUTION.md) — private relay handoff, config,
   key ceremony and operator checklist.
2. Give [docs/AGENT_DEPLOYMENT.md](docs/AGENT_DEPLOYMENT.md) to an LLM/agent
   assisting an operator.
3. Use [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) for a single-host reference.

All three operators must share the same chain, vault/redeem definition,
threshold and `P2P_GROUP`. Each uses a unique `KEEPER_SELF_INDEX` and their own
WIF.

## CI/CD

- `.github/workflows/ci.yml` runs `go vet`, short tests, and cross-compiles
  every release target on pushes and pull requests.
- `.github/workflows/release.yml` runs on `v*` tags (or manual dispatch),
  builds Linux amd64/arm64/armv7, macOS Intel/Apple Silicon, and Windows
  amd64/ARM64, packages them, and publishes a GitHub Release.
- `scripts/package-release.sh` creates the same archives locally from a
  directory of prebuilt binaries.

## Releases and the distribution archive

The repository contains source only. Release archives are built from a clean
tree and contain the static binary, example config, systemd unit, installer and
the three guides. They contain **no** WIF, password, runtime state or live
wallet address. Relay and peer information in the guides is public
infrastructure, not a secret.

## License

No license is granted by default. Contact the maintainers before redistribution
or commercial use.
