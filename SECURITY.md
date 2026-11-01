# Security Policy

## Reporting a vulnerability

Please open a **private security advisory** on this repository
(Security → Advisories → Report a vulnerability). Do not publish an exploit or
a live key in a public issue.

Include:

- affected version/commit,
- a short reproduction,
- impact (funds, key exposure, denial of service),
- any suggested fix.

## Operational security rules

- **Never commit** `dedoo-mm.env`, `keeper-keys.json`, `keeper-state.json`,
  `keeper-accounts.json`, `keeper-identity.bin`, a WIF, or a screenshot that
  shows a private key.
- Each operator holds only their own WIF. Never send a WIF to another operator,
  an agent, a chat, a ticket, or a shell command line.
- Keep the API on `127.0.0.1` with an SSH tunnel, or bind it to a trusted LAN
  address with Basic Auth. Never expose it directly to the public internet.
- Keep `AUTO_SIGN=false` and `AUTO_BROADCAST=false` unless the operators
  explicitly accept the risk.
- Verify destination, amount, change, inputs and fee in the UI before
  approving a proposal.
- Back up `keeper-state.json`, `keeper-accounts.json` and `keeper-keys.json`
  (`0600`) before upgrades. Treat the keys backup as a secret.
- If a WIF may have leaked, stop the node, create a new wallet with a fresh key
  ceremony, and move funds with the old 2-of-3 approval. Do not reuse the key.

## Scope

This project signs and broadcasts transactions. A vulnerability that lets a
single operator sign alone, bypasses proposal verification, exposes a WIF, or
changes a destination/amount after approval is considered critical.
