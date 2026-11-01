# Dedoo Multisig Manager — Agent Deployment Guide

Instructions for an LLM/agent helping a key holder deploy one WojakCoin
mainnet keeper node. The agent must follow this document, the operator's
explicit instructions, and the security rules below.

---

## Role and hard rules

You are assisting **one operator** with **one keeper node**. The other two
operators are independent; never ask for, accept, log, or transmit their
private keys.

**Never**

- ask the operator to paste a WIF into chat, a shell command line, a log, or a
  ticket; have them edit `dedoo-mm.env` locally or use the UI;
- print file contents that contain `KEEPER_WIF`, `API_AUTH_PASS`, or
  `keys.json`;
- transfer, sign or broadcast mainnet funds without an explicit request;
- disable TLS/auth, expose the API publicly, or open unnecessary ports;
- change the chain of an existing node or mix two chains in one process;
- use JKC-testnet/test keys on mainnet.

**Always**

- work on the operator's own host with their SSH key;
- keep `dedoo-mm.env` and `keys.json` at `0600`, owned by `dedoo`;
- verify `VAULT_ADDRESS`, `REQUIRED_SIGS` and WIF version before starting;
- keep `AUTO_SIGN=false`, `AUTO_BROADCAST=false`;
- ask the operator to approve every on-chain action;
- verify that all three nodes show the same wallet address before funding;
- treat the refund/recovery address as an operator decision, never guess it;
- report the exact peer ID/multiaddr and validation output back to the operator.

---

## Inputs to collect

Ask the operator for these values. Do not guess any of them.

| Input | Notes |
|---|---|
| Host / SSH target | root + SSH key |
| Keeper index | 0, 1 or 2 — unique in the trio |
| WojakCoin WIF | operator-entered only; WIF version 201 |
| Vault P2SH address | shared by all three operators |
| Ceremony pubkeys + key order | shared, or shared redeem script hex |
| Required signatures | 2 for this deployment |
| P2P group | shared string, e.g. `wojak-mainnet-vault1` |
| Relay multiaddr | provided privately by the coordinator; never commit it |
| API password | generate a long random value; store on the host only |
| Public host/IP | used to publish this keeper's multiaddr |

Relay details:

- host/peer ID/multiaddr: provided privately by the coordinator
- ports: TCP 4000 + UDP 4000
- role: bootstrap relay only; never send it keys or wallet config
- do not write the relay address into the repository, a release, a public
  issue, or a screenshot

---

## Preflight (read-only)

Run and record:

```bash
uname -m
cat /etc/os-release | head -3
systemctl is-system-running
df -h / | tail -1
free -h | head -2
ss -ltnup | grep -E ':4000|:8090' || true
command -v curl jq sha256sum
```

Expected: x86_64 Linux, systemd running, ≥100 MB free, ports 4000/8090 free.

Confirm outbound connectivity:

```bash
curl -sS -m 10 https://wojak-api.s3na.xyz/blocks/tip/height
nc -vz -w 5 <RELAY_HOST> 4000
```

Do not proceed if the electrs endpoint or relay port is unreachable.

---

## Deploy

1. Upload the package and install:

```bash
VERSION=v0.1.1
scp "dedoo-multisig-manager-${VERSION}-linux-amd64.tar.gz" root@HOST:/root/
ssh root@HOST
cd /root
tar xzf "dedoo-multisig-manager-${VERSION}-linux-amd64.tar.gz"
cd "dedoo-multisig-manager-${VERSION}-linux-amd64"
sha256sum -c SHA256SUMS
./install.sh ./dedoo-mm .
```

2. The operator edits `/opt/dedoo-mm/dedoo-mm.env` (never paste the WIF in
   chat). Required shape:

```ini
CHAIN="wojakcoin"
KEEPER_MODE="keeper"
KEEPER_SELF_INDEX="<0|1|2>"
KEEPER_WIF="<operator WIF>"

VAULT_ADDRESS="<shared vault>"
REQUIRED_SIGS="2"
CEREMONY_PUBKEYS="<shared pub0,pub1,pub2>"
KEY_ORDER="0,1,2"
# or REDEEM_SCRIPT_HEX="<shared redeem>"

BACKEND="electrs"
ELECTRS_URL="https://wojak-api.s3na.xyz"
COINBASE_MATURITY="20"
MIN_TRANSFER_SATS="10000"
# Operator cap. The chain's 100 KB standard transaction limit allows about
# 333 P2SH 2-of-3 inputs; raise this if you consolidate more per transaction.
MAX_TX_INPUTS="100"
AUTO_SIGN="false"
AUTO_BROADCAST="false"

P2P_BIND="0.0.0.0"
P2P_PORT="4000"
P2P_PUBLIC_DHT="true"
P2P_GROUP="wojak-mainnet-vault1"
P2P_BOOTSTRAP="<relay multiaddr provided privately>"

API_LISTEN="127.0.0.1:8090"
API_AUTH_USER="holder"
API_AUTH_PASS="<generated>"
STATE_FILE="/opt/dedoo-mm/keeper-state.json"
ACCOUNTS_FILE="/opt/dedoo-mm/keeper-accounts.json"
KEYS_FILE="/opt/dedoo-mm/keeper-keys.json"
```

3. Permissions:

```bash
chown dedoo:dedoo /opt/dedoo-mm/dedoo-mm.env
chmod 600 /opt/dedoo-mm/dedoo-mm.env
```

4. Firewall (public VPS only):

```bash
ufw allow 4000/tcp
ufw allow 4000/udp
```

5. Start and validate:

```bash
systemctl enable --now dedoo-mm
sleep 3
systemctl is-active dedoo-mm
journalctl -u dedoo-mm -n 40 --no-pager
```

Success criteria in the log:

- `vault address matches redeem script: ... (2-of-3)`
- `network consensus checksum: ...`
- `Host PeerID: 12D3KooW...`
- `connected static peer ... relay ...`
- `keeper #N running on ...`

6. API validation (do not print the password):

```bash
curl -u holder:$API_PASS -s http://127.0.0.1:8090/api/status | jq
curl -u holder:$API_PASS -s http://127.0.0.1:8090/api/accounts | jq
curl -u holder:$API_PASS -s http://127.0.0.1:8090/api/keys | jq
```

Verify:

- `config.chainName == "wojakcoin"`
- `config.vaultAddress` matches the shared vault
- `config.requiredSignatures == 2`
- `config.walletHasKey == true` for the env vault
- `peers` eventually reaches 2

7. UI access:

```bash
ssh -L 8090:127.0.0.1:8090 root@HOST
# open http://127.0.0.1:8090
```

If the operator explicitly confirms the host sits on a **trusted private LAN**
(no public IP), the API may bind to the LAN address instead:

```ini
API_LISTEN="192.168.x.y:8090"
```

Then the UI is reachable at `http://192.168.x.y:8090` from that LAN. Do not
expose 8090 on a public interface without TLS + authentication.

---

### Manager-first alternative

You may leave `KEEPER_WIF`, `VAULT_ADDRESS` and the redeem/pubkey fields empty
and start the service. The node runs as a wallet manager (watch-only) and the
API/UI come up. Then:

1. open the UI,
2. create the shared multisig wallet (3 pubkeys / redeem, threshold, order),
3. add the operator's WIF under Wallets → Signing keys.

The node starts signing as soon as a matching key is added. This avoids
putting a WIF in `dedoo-mm.env` at all. All three operators must add the same
wallet definition.

---

## Adding wallets and keys (after deployment)

Each operator must add the **same wallet definition** on their node; only the
private keys differ.

- UI: Wallets → Create wallet (3 pubkeys / redeem, threshold, order).
- UI: Wallets → Signing keys → add the operator's WIF.
- UI: Use / sidebar selector to switch wallets.
- A key signs every wallet whose redeem contains its pubkey; unkeyed wallets
  are watch-only.

Ceremony checklist (all three operators):

1. Collect the three compressed public keys; never collect WIFs.
2. Agree on key order and threshold (2-of-3).
3. Create the wallet on one node, copy its config, and have the other two
   create the same definition.
4. Compare the derived P2SH address on all three nodes; it must match exactly.
5. Only then fund the wallet. A mismatch means a wrong key or order — stop.

Never put a WIF in a chat, ticket, shell history, or a command line. Use the UI
or a temporary `0600` file deleted immediately after use.

If using the API, never put a WIF in shell history or logs. Use the UI, or a
temporary `0600` JSON file with `curl --data @file` that is deleted immediately.

---

## Success report template

Return this to the operator (no secrets):

```
Node: <hostname> (<public IP>)
Chain: wojakcoin
Keeper index: <N>
Peer ID: 12D3KooW...
Public multiaddr: /ip4/<ip>/tcp/4000/p2p/12D3KooW...
Relay: <provided privately>
P2P group: wojak-mainnet-vault1
Vault: <P2SH>
Threshold: 2-of-3
Service: active (enabled)
Peers: <N>
API: 127.0.0.1:8090 (SSH tunnel only)
UI login: holder (password stored on host)
Backups: state/accounts/keys paths confirmed
```

---

## Failure handling

| Failure | Action |
|---|---|
| Vault/redeem mismatch | Do not start. Ask operators to re-check shared config. |
| `REQUIRED_SIGS` mismatch | Same; never edit the redeem script to fit. |
| Wrong WIF version | Wrong chain/key; stop and ask the operator. |
| Relay unreachable | Check outbound TCP/UDP 4000; try `nc`; do not disable auth. |
| Peers stay 0/1 | Check `P2P_GROUP`, relay, firewall, other keepers. |
| Port 4000/8090 busy | Stop conflicting service; choose another API port only. |
| Service restart loop | `journalctl -u dedoo-mm -n 80`; report without secrets. |

Escalate to the operator for any action that could move funds. Deployment
assistance never includes signing or broadcasting without explicit approval.
