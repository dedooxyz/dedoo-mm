# Dedoo Multisig Manager — Distribution Guide

Operator guide for deploying one keeper of a WojakCoin mainnet 2-of-3
multisig vault. This is written for the three key holders.

---

## 0. What is being deployed

```
                    WojakCoin mainnet
                           │
            ┌──────────────┼──────────────┐
            │              │              │
       Keeper #0       Keeper #1      Keeper #2
       (you/VPS)       (member)       (member)
            │              │              │
            └──────── private relay ──────┘
              (address shared privately)
```

- Each keeper runs **Dedoo Multisig Manager** on its own host.
- One node is restricted to **one chain**; this deployment is WojakCoin
  mainnet (`CHAIN=wojakcoin`).
- A node may manage **several multisig wallets** on that chain. A signing key
  is node-wide: one WIF can sign every wallet whose redeem script contains its
  public key.
- The three operators coordinate through `P2P_GROUP` and the relay. Wallet
  definitions (address/redeem) are added on every node; **private keys are
  never shared**.

### Private relay (address not published)

The trio coordinates through a private bootstrap relay. Its host, peer ID and
multiaddr are **not published** in this repository or release. The coordinator
sends them to each operator over a private channel.

| Item | Value |
|---|---|
| Host | `<provided privately>` |
| Peer ID | `<provided privately>` |
| Multiaddr | `<provided privately>` |
| Ports | TCP **4000** and UDP **4000** |
| Mode | bootstrap relay only — no keys, no vault |

The relay runs Circuit Relay v2 + NAT/hole-punching support. Keep it online;
it does not sign anything. Never commit the relay address to a public
repository or paste it into a public issue.

### Trio coordination table (fill before deployment)

| Operator | Keeper index | Public host | Peer ID | Public multiaddr |
|---|---|---|---|---|
| | 0 | | | |
| | 1 | | | |
| | 2 | | | |

All three must share:

- `CHAIN=wojakcoin`
- `VAULT_ADDRESS` (same P2SH vault)
- `REQUIRED_SIGS` (2)
- `CEREMONY_PUBKEYS` + `KEY_ORDER`, or `REDEEM_SCRIPT_HEX`
- `P2P_GROUP` (e.g. `wojak-mainnet-vault1`)
- `P2P_BOOTSTRAP` (relay multiaddr)

Each operator supplies only their own:

- unique `KEEPER_SELF_INDEX` (0, 1, or 2)
- their `KEEPER_WIF`
- their host/SSH access
- their own API password

### Shared wallet definition (key ceremony)

Do this once, before funding anything:

1. Each operator generates a key **offline** and keeps the WIF private.
2. Each operator shares only the **compressed public key** (33-byte hex).
3. One operator opens the UI → Wallets → Create wallet, enters the three
   public keys in a agreed order (usually `0,1,2`), sets `2-of-3`, and saves.
4. That operator uses **Copy config** and shares `VAULT_ADDRESS`,
   `REQUIRED_SIGS`, `CEREMONY_PUBKEYS` and `KEY_ORDER` with the other two.
5. The other two create the same wallet (same keys and order) and must see the
   **same P2SH address**. If the address differs, stop and re-check the keys
   and order.
6. Each operator adds only their own WIF under Wallets → Signing keys.

Never send a WIF, `keys.json`, or a screenshot containing a private key to the
group. The redeem script and P2SH address are public and safe to share.

---

## 1. Package

```bash
dedoo-multisig-manager-linux-amd64/   (top level of the tarball)
├── wojak-keeper            # static linux/amd64 binary
├── keeper.env.example
├── wojak-keeper.service
├── install.sh
├── DEPLOYMENT.md
├── DISTRIBUTION.md         # this guide
├── AGENT_DEPLOYMENT.md     # LLM/agent-assisted deployment
└── SHA256SUMS
```

Verify:

```bash
sha256sum -c SHA256SUMS
```

The technical binary/service names keep the historical `wojak-keeper` prefix
for upgrade compatibility; the product is Dedoo Multisig Manager.

---

## 2. Host requirements

- Linux x86_64, systemd, root SSH key access
- Outbound HTTPS to `https://wojak-api.s3na.xyz` (WojakCoin electrs/Esplora)
- Outbound TCP/UDP 4000 to the relay
- Inbound P2P port 4000 only if this node has a public IP (optional; the relay
  covers NAT/home/LXC nodes)
- ~100 MB disk, 512 MB RAM

Recommended hostname: `keeper-<index>.<your-domain>`.

---

## 3. Install

```bash
scp dedoo-multisig-manager-linux-amd64.tar.gz root@YOUR_HOST:/root/
ssh root@YOUR_HOST
cd /root
tar xzf dedoo-multisig-manager-linux-amd64.tar.gz
cd dedoo-multisig-manager-linux-amd64
./install.sh ./wojak-keeper .
```

The installer creates:

- `/opt/wojak-keeper/wojak-keeper`
- `/opt/wojak-keeper/keeper.env` (template, never overwritten on upgrade)
- user `wojakkeeper`
- unit `wojak-keeper.service` (do not start yet for mainnet unless configured)

---

## 4. Configure WojakCoin mainnet

Edit `/opt/wojak-keeper/keeper.env` **on the operator host only**:

```ini
### Chain — one chain per node
CHAIN="wojakcoin"
KEEPER_MODE="keeper"
KEEPER_SELF_INDEX="0"                 # 0, 1, or 2 — unique in the trio
KEEPER_WIF="<your mainnet WIF>"

### Shared multisig vault
VAULT_ADDRESS="<P2SH vault address>"
REQUIRED_SIGS="2"
CEREMONY_PUBKEYS="<pub0>,<pub1>,<pub2>"
KEY_ORDER="0,1,2"
# or, instead of CEREMONY_PUBKEYS:
# REDEEM_SCRIPT_HEX="522102...53ae"

### Chain backend
BACKEND="electrs"
ELECTRS_URL="https://wojak-api.s3na.xyz"
COINBASE_MATURITY="20"
MIN_TRANSFER_SATS="10000"
MAX_TX_INPUTS="100"

### Human-in-the-loop
AUTO_SIGN="false"
AUTO_BROADCAST="false"

### P2P — all three keepers must share P2P_GROUP and the relay
P2P_BIND="0.0.0.0"
P2P_PORT="4000"
P2P_PUBLIC_DHT="true"
P2P_GROUP="wojak-mainnet-vault1"
P2P_BOOTSTRAP="<relay multiaddr provided privately>"
# Optional static peers (recommended once public identity is known):
# P2P_PEERS="/ip4/<other1>/tcp/4000/p2p/<ID1>,/ip4/<other2>/tcp/4000/p2p/<ID2>"

### API / persistence
API_LISTEN="127.0.0.1:8090"
API_AUTH_USER="holder"
API_AUTH_PASS="<long random password>"
STATE_FILE="/opt/wojak-keeper/keeper-state.json"
ACCOUNTS_FILE="/opt/wojak-keeper/keeper-accounts.json"
KEYS_FILE="/opt/wojak-keeper/keeper-keys.json"
LOG_LEVEL="info"
```

Permissions:

```bash
chown wojakkeeper:wojakkeeper /opt/wojak-keeper/keeper.env
chmod 600 /opt/wojak-keeper/keeper.env
```

When the env vault is configured, startup refuses to run unless:

- `VAULT_ADDRESS` equals P2SH(redeem script)
- `REQUIRED_SIGS` equals the redeem script threshold
- `KEEPER_WIF` uses the chain WIF version (201 for WojakCoin)

That is intentional: a misconfigured node must never sign. If those fields are
empty, the node starts in manager mode (see below) and can be configured from
the UI.

---


### Manager-first alternative

You may leave `KEEPER_WIF`, `VAULT_ADDRESS` and the redeem/pubkey fields empty
and start the service. The node runs as a wallet manager (watch-only) and the
API/UI come up. Then:

1. open the UI,
2. create the shared multisig wallet (3 pubkeys / redeem, threshold, order),
3. add the operator's WIF under Wallets → Signing keys.

The node starts signing as soon as a matching key is added. This avoids
putting a WIF in `keeper.env` at all. All three operators must add the same
wallet definition.

## 5. Firewall

Public VPS (recommended):

```bash
ufw allow 4000/tcp
ufw allow 4000/udp
ufw status
```

Home/LXC behind NAT: outbound access to the relay is enough; AutoRelay and
hole punching establish connectivity. Port-forwarding TCP+UDP 4000 is optional
and only improves direct connections.

Never expose `API_LISTEN` publicly without TLS + auth.

---

## 6. Start and verify

```bash
systemctl enable --now wojak-keeper
systemctl status wojak-keeper --no-pager
journalctl -u wojak-keeper -f
```

Expected:

```
[main] vault address matches redeem script: 3... (2-of-3)
[main] network consensus checksum: ...
[p2p] Host PeerID: 12D3KooW...
[p2p] connected static peer <relay-peer-id> (relay)
[main] keeper #0 running on ... (api 127.0.0.1:8090, ...)
```

API checks (local):

```bash
curl -u holder:PASSWORD -s http://127.0.0.1:8090/api/status | jq
curl -u holder:PASSWORD -s http://127.0.0.1:8090/api/accounts | jq
curl -u holder:PASSWORD -s http://127.0.0.1:8090/api/keys | jq
```

Web UI (public VPS — keep the API on loopback and tunnel):

```bash
ssh -L 8090:127.0.0.1:8090 root@YOUR_HOST
# open http://127.0.0.1:8090
```

Private LAN (trusted network only): bind the API to the node's LAN address
instead, then open it directly from another machine on that LAN:

```ini
API_LISTEN="192.168.x.y:8090"
```

```bash
# from a workstation on the same LAN
# open http://192.168.x.y:8090
```

Never do this on a host with a public IP. Keep `API_AUTH_USER` /
`API_AUTH_PASS` set in both cases.

Invite the other two keepers:

1. They deploy the same package and set the same shared values.
2. Each uses their own index and WIF.
3. Confirm each node sees `peers 2` (or at least the relay) in the sidebar.
4. If using static peers, each lists all other nodes in `P2P_PEERS`.

---

## 7. Add wallets, keys and switch wallets

The UI is designed for several multisig wallets on this one chain.

- **Wallets → Create wallet**: enter 3 public keys (or a redeem script),
  threshold and key order. The address is derived locally. Repeat on **all
  three nodes** with the same definition.
- **Wallets → Signing keys**: add your own WIF. One key can sign every wallet
  whose redeem script contains that public key.
- **Wallets → Use** or the sidebar wallet selector: switch the active view.
  Balances, proposals, history, transfers and UTXOs follow the selected wallet.
- Watch-only wallets can be tracked and funded but cannot approve proposals
  until a matching key is added.
- The env vault (`VAULT_ADDRESS` + `KEEPER_WIF`) is always present; additional
  wallets are stored in `ACCOUNTS_FILE`, keys in `KEYS_FILE`.

---

## 8. Backup and upgrade

```bash
systemctl stop wojak-keeper
cp /opt/wojak-keeper/keeper-state.json /root/keeper-state-$(date +%F).json
cp /opt/wojak-keeper/keeper-accounts.json /root/keeper-accounts-$(date +%F).json
cp /opt/wojak-keeper/keeper-keys.json /root/keeper-keys-$(date +%F).json  # secret
systemctl start wojak-keeper
```

Upgrade:

```bash
scp wojak-keeper root@YOUR_HOST:/root/
ssh root@YOUR_HOST
install -m 0755 /root/wojak-keeper /opt/wojak-keeper/wojak-keeper.new
mv -f /opt/wojak-keeper/wojak-keeper.new /opt/wojak-keeper/wojak-keeper
systemctl restart wojak-keeper
```

Config, wallets, keys and proposals are preserved.

---

## 8b. Health monitoring and recovery

Basic checks:

```bash
systemctl is-active wojak-keeper
journalctl -u wojak-keeper -n 100 --no-pager
curl -u holder:PASSWORD -s http://127.0.0.1:8090/api/status | jq '{tipHeight, peers, balanceSats, utxoCount}'
```

Healthy signs: service `active`, chain tip advancing, `peers` reaching 2 (or
the relay at minimum), and wallet balances matching what all three operators
see.

Disaster recovery on a replacement host:

1. Install the same package and restore `keeper.env` (0600).
2. Restore `keeper-state.json`, `keeper-accounts.json` and `keeper-keys.json`
   from backup (0600, owned by `wojakkeeper`).
3. Start the service and confirm the same wallet address and peers.
4. If a WIF may have leaked, do **not** reuse it: create a new wallet with a
   fresh key ceremony and move funds with the old 2-of-3 approval.

---

## 9. Security rules

- Never share or transmit a `KEEPER_WIF` or `keys.json`; each operator imports
  only their own key.
- `keeper.env` and `keys.json` are `0600`, owned by `wojakkeeper`.
- Public host: keep the API on `127.0.0.1` and use an SSH tunnel or TLS
  reverse proxy. Trusted private LAN only: bind to the node's LAN IP and keep
  Basic Auth enabled.
- Verify each proposal in the UI before approving; check destination, amount,
  change, inputs and fee.
- Keep `AUTO_SIGN=false` and `AUTO_BROADCAST=false`.
- Two independent signatures are required before broadcast.
- Back up `state`, `accounts` and `keys` before upgrades.

---

## 10. Troubleshooting

| Symptom | Fix |
|---|---|
| `VAULT_ADDRESS ... does not match the redeem script` | Wrong vault/redeem/pubkeys/order |
| `REQUIRED_SIGS=3 does not match ... threshold 2` | Shared config mismatch |
| `WIF version byte 239, want 201` | JKC testnet key on Wojak mainnet node |
| `peers 0/1` | Relay unreachable; check outbound 4000; `P2P_GROUP` mismatch |
| `watch-only` wallet | Add your WIF in Wallets → Signing keys |
| `unknown wallet` on a proposal | The wallet was not added on this node |
| 429 from electrs | Client retries; check endpoint |
| Service exits after start | `journalctl -u wojak-keeper -n 50` |

---

## 11. Agent/LLM assistance

Give the operator's agent `AGENT_DEPLOYMENT.md` from this package. It contains
a guarded checklist, exact commands, validation criteria and a handoff
template. The agent must never receive another operator's private key.
