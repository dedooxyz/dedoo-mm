> Deploying one node of an operator trio? Start with **DISTRIBUTION.md**
> (private relay handoff, shared config, operator checklist) and give
> **AGENT_DEPLOYMENT.md** to an LLM/agent assisting the operator.

# Dedoo Multisig Manager Deployment Guide

Single 2-of-3 (M-of-3) multisig vault, managed by three independent keeper
nodes with human approval. This guide covers a Linux + systemd host.

Supported chain profiles: **wojakcoin** (default) and **junkcoin**, plus a
fully custom profile (`CHAIN=custom`) where every chain parameter comes from
`.env`.

---

## 0. Package layout

```
wojak-keeper-linux-amd64/
├── wojak-keeper              # static binary
├── keeper.env.example        # configuration template
├── wojak-keeper.service      # systemd unit
├── install.sh                # installer (root)
├── DEPLOYMENT.md             # this guide
└── SHA256SUMS
```

The installer creates:

- `/opt/wojak-keeper/wojak-keeper` — binary
- `/opt/wojak-keeper/keeper.env` — config (never overwritten on upgrade)
- `/opt/wojak-keeper/keeper-state.json` — persisted proposals
- user/group `wojakkeeper`
- unit `/etc/systemd/system/wojak-keeper.service`

---

## 1. Prerequisites

- Linux x86_64, systemd, root access (SSH key recommended)
- Outbound HTTPS to the chain's Esplora/electrs endpoint
- Inbound TCP **and UDP** on the P2P port (default `4000`)
- The operator WIF key and the ceremony data (3 pubkeys or redeem script)

---

## 2. Install

Copy the package to the server and install:

```bash
scp -r wojak-keeper-linux-amd64 root@YOUR_HOST:/root/
ssh root@YOUR_HOST
cd /root/wojak-keeper-linux-amd64
./install.sh ./wojak-keeper .
```

Verify the binary:

```bash
/opt/wojak-keeper/wojak-keeper --help 2>&1 | head -5 || true
sha256sum -c SHA256SUMS
```

---

## 3. Configure

```bash
nano /opt/wojak-keeper/keeper.env
```

### 3.1 Safe first boot (relay only, no key needed)

Start as a bootstrap relay while you prepare the signing key:

```ini
CHAIN="wojakcoin"
KEEPER_MODE="bootstrap"
P2P_PORT="4000"
API_LISTEN="127.0.0.1:8080"
```

```bash
systemctl start wojak-keeper
systemctl status wojak-keeper --no-pager
journalctl -u wojak-keeper -n 30 --no-pager
```

### 3.2 Going live (keeper mode)

When the WIF and ceremony data are ready:

```ini
CHAIN="wojakcoin"                 # or "junkcoin"
KEEPER_MODE="keeper"
KEEPER_SELF_INDEX="0"             # 0, 1, or 2 — unique per operator
KEEPER_WIF="<your WIF>"

# Launch wallet (more wallets can be added in the UI later)
VAULT_ADDRESS="<P2SH vault address>"
REQUIRED_SIGS="2"                 # M in M-of-3

# Redeem script: Option A (derive) or Option B (explicit)
CEREMONY_PUBKEYS="<pub0>,<pub1>,<pub2>"
KEY_ORDER="0,1,2"
# REDEEM_SCRIPT_HEX="522102...53ae"

BACKEND="electrs"
ELECTRS_URL="https://wojak-api.s3na.xyz"   # JKC: https://junk-api.s3na.xyz

COINBASE_MATURITY="20"            # WJK 20 / JKC 70 (clamped to chain minimum)
MIN_TRANSFER_SATS="10000"
MAX_TX_INPUTS="100"

AUTO_SIGN="false"
AUTO_BROADCAST="false"

P2P_BIND="0.0.0.0"
P2P_PORT="4000"
P2P_PUBLIC_DHT="true"
# Static trio instead of DHT:
# P2P_PEERS="/ip4/<node0>/tcp/4000/p2p/<ID0>,/ip4/<node1>/tcp/4000/p2p/<ID1>,/ip4/<node2>/tcp/4000/p2p/<ID2>"

API_LISTEN="127.0.0.1:8080"
STATE_FILE="/opt/wojak-keeper/keeper-state.json"
```

Startup refuses to run if any of these do not agree:

- `VAULT_ADDRESS` must equal P2SH(redeem script)
- `REQUIRED_SIGS` must equal the threshold encoded in the redeem script
- `KEEPER_WIF` must use the chain's WIF version (WJK 201, JKC 144)

```bash
systemctl restart wojak-keeper
journalctl -u wojak-keeper -f
```

Expected log lines:

```
[main] vault address matches redeem script: 3... (2-of-3)
[main] network consensus checksum: ...
[p2p] Host PeerID: 12D3KooW...
[main] keeper #0 running on ... (api 127.0.0.1:8080, vault 3...)
```

---

## 4. Firewall

Only the P2P port must be reachable by the other keepers. Keep the API local.

```bash
# ufw
ufw allow 4000/tcp
ufw allow 4000/udp
ufw status

# firewalld
firewall-cmd --permanent --add-port=4000/tcp --add-port=4000/udp
firewall-cmd --reload
```

If you change `P2P_PORT`, open the same TCP+UDP port.

---

## 5. How key holders access the web UI

Each holder runs **their own keeper node** and opens **that node's** UI. There
is no shared central UI and no user database: access control is either SSH or
HTTP Basic Auth.

### Method A (recommended): SSH tunnel, no web password

The API binds to `127.0.0.1`, so only someone with an SSH key on the VPS can
reach it. The SSH key **is** the authentication.

```bash
# on the holder's laptop
ssh -L 8090:127.0.0.1:8090 root@YOUR_HOST
# then open http://127.0.0.1:8090
```

Every holder does this against their own VPS. Nothing on the keeper is exposed
to the internet except the P2P port.

### Method B: browser access with user + password

If holders cannot use SSH tunnels, enable Basic Auth and put TLS in front:

```ini
# /opt/wojak-keeper/keeper.env
API_LISTEN="127.0.0.1:8090"
API_AUTH_USER="holder"
API_AUTH_PASS="a-long-random-password"
```

```bash
systemctl restart wojak-keeper
```

Then expose it with Caddy (automatic HTTPS) and keep the API itself local:

```caddyfile
keeper.example.com {
    reverse_proxy 127.0.0.1:8090
}
```

The browser will prompt for the user/password you configured. Do **not** open
`8090` in the firewall and do not serve plain HTTP: the keeper API has no TLS
of its own, and the WIF never leaves the server but signed transactions and
approval actions are sensitive.

Useful endpoints (same auth):

```bash
curl -u holder:password -s http://127.0.0.1:8090/api/status | jq
curl -u holder:password -s http://127.0.0.1:8090/api/proposals | jq
curl -u holder:password -s http://127.0.0.1:8090/api/utxos | jq
```

> Note: in `KEEPER_MODE=bootstrap` the API is not started (relay only). The UI
> becomes available after you switch to `keeper` with the WIF/vault config.

---

## 6. Joining the trio

Each node must use the **same** `CHAIN`, `VAULT_ADDRESS`, redeem script,
`REQUIRED_SIGS` and therefore the same consensus checksum (logged at startup).

Two discovery modes:

1. **Public DHT** (default): `P2P_PUBLIC_DHT="true"`; nodes find each other by
   the consensus checksum namespace. May take up to a minute.
2. **Static mesh** (recommended for VPS): set `P2P_PUBLIC_DHT="false"` and list
   **all three** node multiaddrs in `P2P_PEERS` on **every** node.

Read each node's peer ID from the logs:

```
[p2p] Host PeerID: 12D3KooW...
```

---

## 7. Daily operation

1. One holder submits a transfer (or full consolidation) in the UI.
2. The other two nodes show it as **PENDING** and verify inputs/outputs.
3. Each holder presses **Approve & Sign** (or **Reject**, informational).
4. At M signatures the proposal becomes **READY**.
5. Any holder presses **Broadcast to chain** — once.

`AUTO_SIGN` / `AUTO_BROADCAST` restore the legacy fully-automatic flow and are
not recommended.

---

## 7b. Multi-wallet operations (one chain per node)

One node is deliberately restricted to **one chain** (`CHAIN` in keeper.env).
Within that chain it can manage several multisig wallets at the same time:

- **Create wallet** on the Wallets page from 3 public keys or a redeem script.
- **Add private key** once: a signing key belongs to the node, not to one
  wallet. It can sign every wallet whose redeem script contains that public
  key (typical when the same ceremony keys secure several vaults).
- **Switch wallet** in the sidebar (or the Use button). Balances, proposals,
  history, transfers and UTXOs follow the selected wallet.
- Wallets without a matching key are **watch-only**: they can still be tracked,
  funded and verified, but this node cannot approve proposals for them.

Saved wallets live in `ACCOUNTS_FILE`; imported WIFs live in `KEYS_FILE`
(both `0600`, never returned by the API). The env vault (`VAULT_ADDRESS` +
`KEEPER_WIF`) is always present and cannot be removed from the UI.

To operate a wallet on a **different chain**, deploy a separate keeper with
that chain's config — a single process never mixes chains.

## 8. Backup and upgrade

State is a single JSON file:

```bash
systemctl stop wojak-keeper
cp /opt/wojak-keeper/keeper-state.json /root/keeper-state-$(date +%F).json
cp /opt/wojak-keeper/keeper-accounts.json /root/keeper-accounts-$(date +%F).json
systemctl start wojak-keeper
```

Upgrade (config and state are preserved):

```bash
scp wojak-keeper root@YOUR_HOST:/root/
ssh root@YOUR_HOST
install -m 0755 /root/wojak-keeper /opt/wojak-keeper/wojak-keeper.new
mv -f /opt/wojak-keeper/wojak-keeper.new /opt/wojak-keeper/wojak-keeper
systemctl restart wojak-keeper
```

Protocol note: keeper P2P uses `/wojak-keeper/v3`; all three nodes must run the
same protocol version.

---

## 9. Security checklist

- [ ] `keeper.env` mode `0600`, owner `wojakkeeper`
- [ ] Public host: API bound to `127.0.0.1`, accessed via SSH tunnel
- [ ] Trusted LAN only: API bound to the LAN IP, Basic Auth enabled
- [ ] P2P port open; all other ports closed
- [ ] SSH key-only auth, root login restricted where possible
- [ ] Offline backup of the WIF and ceremony data
- [ ] State file backed up before upgrades
- [ ] `AUTO_SIGN=false`, `AUTO_BROADCAST=false`

---

## 10. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `VAULT_ADDRESS ... does not match the redeem script` | Wrong vault address or wrong pubkeys/order/redeem hex |
| `REQUIRED_SIGS=3 does not match ... threshold 2` | `REQUIRED_SIGS` differs from the redeem script |
| `WIF version byte 201, want 144` | Wrong chain selected or wrong key |
| `no mature utxos` | Vault empty or outputs below `COINBASE_MATURITY` |
| `peers 0` / `peers 1` | Firewall UDP/TCP, wrong `P2P_PEERS`, peers not running |
| `electrs HTTP status 429` | Rate limited; the client retries automatically |
| UI shows 0 balance | Wrong `VAULT_ADDRESS`/chain endpoint |
| Service exits immediately | `journalctl -u wojak-keeper -n 50 --no-pager` |

Uninstall:

```bash
systemctl disable --now wojak-keeper
rm -f /etc/systemd/system/wojak-keeper.service
systemctl daemon-reload
rm -rf /opt/wojak-keeper
userdel wojakkeeper
```
