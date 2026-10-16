<p align="center">
  <img src="docs/assets/hero.svg" alt="CryptoPayments — Receive. Verify. Notify. USDT BEP20 payment infrastructure in Go." width="100%">
</p>

<p align="center">
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&amp;logo=go&amp;logoColor=white" alt="Go 1.25"></a>
  <img src="https://img.shields.io/badge/Network-BNB_Smart_Chain-F0B90B?style=flat-square" alt="BNB Smart Chain">
  <img src="https://img.shields.io/badge/Asset-USDT_BEP20-26A17B?style=flat-square" alt="USDT BEP20">
  <img src="https://img.shields.io/badge/Storage-SQLite_WAL-4169E1?style=flat-square" alt="SQLite WAL">
  <img src="https://img.shields.io/badge/API-Watch--only-8B5CF6?style=flat-square" alt="Watch-only API">
</p>

<h1 align="center">CryptoPayments</h1>

<p align="center">
  <strong>Your invoices. Your addresses. Your payment infrastructure.</strong><br>
  A self-hosted Go API that receives and verifies USDT payments on BNB Smart Chain.<br>
  Create an invoice, receive funds at a unique address, and get confirmed status or a signed webhook.
</p>

<p align="center">
  <a href="#quick-local-test--no-funds-or-faucet-needed">Quick start</a> ·
  <a href="#architecture">Architecture</a> ·
  <a href="#payment-flow">Payment flow</a> ·
  <a href="#api">API</a> ·
  <a href="openapi.yaml">OpenAPI contract</a> ·
  <a href="#webhooks">Webhooks</a> ·
  <a href="#automatic-collection-to-your-main-wallet">Automatic collection</a>
</p>

---

## Built for a clear payment boundary

| Receive | Verify | Notify |
| :--- | :--- | :--- |
| Fresh HD deposit address for every invoice | Configured token, recipient, confirmations **and** finality | Poll invoice status or consume signed webhooks |
| Idempotent `order_id` + amount | Exact 18-decimal integer arithmetic | Durable outbox, retry/backoff and per-invoice ordering |
| Watch-only API; optional isolated signing worker | Partial transfers accumulate; excess payments count | Persistent invoice snapshots and event IDs |

> **Current scope:** an API-only payment receiver with optional automatic USDT
> collection. A separate signer funds child-wallet BNB gas when needed and sends
> confirmed deposits to `MAIN_WALLET_ADDRESS`. The API never mounts signing keys.
> Public BSC Testnet end-to-end validation and real-money validation are pending.

## Architecture

![CryptoPayments system architecture](docs/diagrams/architecture.svg)

The HTTP API, transfer scanner and webhook dispatcher run in **one Go process**.
SQLite stores invoices, address allocation, confirmed transfers, scan cursors
and the optional webhook outbox. An opt-in signer runs in a **separate process**,
sharing SQLite for collection jobs and persisted signed transactions. BSC
JSON-RPC supplies chain data and accepts the signer’s transaction broadcasts.

The server derives deposit addresses from an xpub at `m/44'/60'/0'/0/<index>`.
`wallet-init` creates the seed separately. When automatic collection is enabled,
only the isolated `sweeper` process mounts that seed and the gas-wallet key.
No signing key is supplied to the API runtime.

[Interactive architecture](docs/diagrams/architecture.html) ·
[Diagram sources & viewing instructions](docs/diagrams/README.md)

## Payment flow

![Invoice creation, transfer confirmation and webhook delivery](docs/diagrams/payment-flow.svg)

1. **Create:** your backend submits an order ID and a string amount using Bearer authentication.
2. **Receive:** the customer sends the configured token to the returned deposit address.
3. **Confirm:** the scanner checks finality, confirmation depth, the token and recipient, and canonical block data.
4. **Commit:** matching transfers, the invoice total, the cursor and any update event commit in one SQLite transaction.
5. **Notify:** your backend polls the invoice or receives an HMAC-signed webhook when configured.

[Interactive payment sequence](docs/diagrams/payment-flow.html)

### The acceptance rule

```text
is_paid = confirmed received amount >= requested amount
```

| Requested | Confirmed deposits | Result | `is_paid` |
| --- | --- | --- | --- |
| 10 USDT | 4 USDT | `partially_paid` | `false` |
| 10 USDT | 4 + 6 USDT | `paid` | `true` |
| 10 USDT | 12 USDT | `overpaid` | `true` |
| 10 USDT | 10 USDT, threshold reached after expiry | `paid_late` | `true` |

Timing uses the transfer block's timestamp. Late payment remains explicit in
`status`; your application decides whether a late order should be fulfilled.
Always deduplicate fulfilment by invoice ID.

## What works

- Unique HD address per invoice; addresses are never reassigned.
- Same order and numeric amount return the existing invoice; a changed amount returns 409.
- JSON amounts are **strings**; all accounting uses integer arithmetic at 18 decimals.
- Partial, exact, excess, expired and late payment states.
- Only the configured chain and token contract can count as payment.
- Both configured confirmation depth **and** RPC `finalized` block are required.
- Durable per-invoice cursors and unique `(tx_hash, log_index)` records.
- Signed webhook outbox with retry/backoff and per-invoice event ordering.
- Mainnet configuration (56), public testnet configuration (97), and isolated local EVM tests.
- Optional automatic gas funding and USDT collection, with persistent signed transactions and restart-safe rebroadcast.

## Quick local test — no funds or faucet needed

Requires Docker Compose and Python 3. If Go 1.25+ and a C compiler are installed:

```sh
go build -o bin/cryptopayments .
python3 scripts/local_test.py setup
python3 scripts/local_test.py test
```

Or build using Docker:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.25-bookworm \
  sh -c 'go build -o bin/cryptopayments .'
python3 scripts/local_test.py setup
python3 scripts/local_test.py test
```

`setup` starts Anvil with chain ID 97, deploys `contracts/TestUSDT.sol`, creates
a separate local wallet, writes an ignored `.env` with a random API key, and
starts the API at **http://127.0.0.1:18090**. The EVM is at port 18545, bound to
loopback. The API is watch-only; it does not mount the wallet seed.

The helper refuses to transact on any node other than local Anvil. Test tokens
and Anvil accounts have **no monetary value**. This is a local EVM test, not a
public BSC Testnet transaction. Never fund Anvil's publicly known keys.

The local EVM and database persist in separate Docker volumes. Keep them
together; do not reset only the chain while retaining its invoice database.
The helper refuses to replace a non-development `.env`.

Tests cover actual token transfers, split payments, overpayment, ignoring a
different token with the same symbol, restart persistence and unique addresses.
Go tests additionally cover finality, confirmation depth, expired/late invoices,
RPC failures, malformed/noncanonical logs, concurrent address allocation and
signed webhook retries.

## API

All `/v1/*` routes require `Authorization: Bearer <API_KEY>`. See
[openapi.yaml](openapi.yaml) for the contract. Default Compose binding is loopback
only. Put authenticated HTTPS ingress in front if external applications need access.
The API key grants access to all invoices in this service; this is not a
multi-merchant authorization system.

| Method | Route | Authentication | Purpose |
| --- | --- | --- | --- |
| `POST` | `/v1/invoices` | Bearer | Create an invoice or replay an existing order |
| `GET` | `/v1/invoices/{id}` | Bearer | Invoice, confirmed transfers and payment status |
| `GET` | `/v1/invoices/{id}/sweeps` | Bearer | Collection jobs, main-wallet destination and gas/token transaction hashes |
| `GET` | `/v1/status` | Bearer | Scanner health, safe block and chain configuration |
| `GET` | `/healthz` | Public | Process liveness |
| `GET` | `/readyz` | Public | Scanner readiness; 503 when unhealthy or stale |

Create an invoice:

```sh
curl http://127.0.0.1:18090/v1/invoices \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"order_id":"shop-order-1001","amount":"25.50"}'
```

Example response, abbreviated:

```json
{
  "id": "inv_...",
  "order_id": "shop-order-1001",
  "address": "0x...",
  "amount": "25.5",
  "received_amount": "0",
  "status": "pending",
  "is_paid": false,
  "network": "bsc-testnet",
  "chain_id": 97,
  "asset": "TEST-USDT",
  "token_contract": "0x...",
  "required_confirmations": 15,
  "expires_at": 1792198800,
  "transfers": []
}
```

Creation returns 201; a retry of the same order and numeric amount returns 200.
A different amount with the same order returns 409. Use a fresh order ID for a
new payment attempt. This also applies after expiry: old addresses are never
reassigned to a different order.

Read invoice / scanner:

```sh
curl -H "Authorization: Bearer $API_KEY" \
  http://127.0.0.1:18090/v1/invoices/INVOICE_ID
curl -H "Authorization: Bearer $API_KEY" \
  http://127.0.0.1:18090/v1/status
```

| Status | Meaning |
| --- | --- |
| `pending` | No confirmed matching transfers yet |
| `partially_paid` | Confirmed total is below the requested amount |
| `paid` | Requested amount reached before expiry |
| `overpaid` | Total exceeds the request; threshold reached before expiry |
| `expired` | Deadline passed with insufficient confirmed payment |
| `paid_late` | Requested amount reached after expiry |
| `overpaid_late` | Overpayment with threshold reached after expiry |

`received_amount` and `transfers` contain **confirmed** transfers only. An
unconfirmed transfer stays `pending` until eligible. Expired and paid addresses
continue to be monitored, so later deposits are recorded. Payment timing uses
the inclusion block's timestamp, not the time the scanner sees it. A transfer
included before expiry can change `expired` to `paid` once confirmed.

Acceptance uses **confirmed received amount >= requested amount**, never exact
equality alone. `is_paid` is `true` for both exact and excess payments (including
late payments). For a request of 10, receiving 10 or 12 is fully paid; receiving
9 is not. Late timing remains visible separately in `status`.

Fulfil an order once for `paid` or `overpaid`, keyed by invoice ID. Handle
`*_late` according to your business rules; do not automatically fulfil an
expired order merely because a late transfer arrives. Increasing an already
paid invoice to `overpaid` must not fulfil it a second time.

`GET /healthz` is process liveness. `GET /readyz` returns 503 if the scanner has
not succeeded, has an error, or is stale. `/v1/status` reports the last scan
time/error and safe block. A scan can succeed while catching up on historical
ranges; compare invoice `next_scan_block` with `safe_block` to assess lag.

## Public BSC Testnet

Use a **separate directory, wallet, env file and database/volume** from local
development and mainnet. Public testnet uses chain ID **97** and test BNB for
transaction gas. [BNB's RPC documentation](https://docs.bnbchain.org/bnb-smart-chain/developers/json_rpc/json-rpc-endpoint/)
lists endpoints; some public endpoints disable `eth_getLogs`. Use a provider
that supports logs and `eth_getBlockByNumber("finalized")`.

1. Generate a separate wallet on a trusted machine:

   ```sh
   umask 077
   bin/cryptopayments wallet-init /secure/offline/testnet-wallet.seed > testnet-wallet.xpub
   ```

   The output is a `WALLET_XPUB=...` assignment. Put only that public value in
   `.env`. Move/back up the seed offline. The seed file is 32 bytes encoded as
   hex (raw BIP32 seed), **not** a mnemonic. Back up the database and derivation
   path too. To restore the branch, use BIP32 seed hex and path `m/44'/60'/0'/0`.

2. Fund a separate test sender/deployer with test BNB using a faucet linked in
   [BNB's faucet documentation](https://docs.bnbchain.org/docs/bsc-faucet/).
   Faucet availability and eligibility vary.
3. Compile and deploy `contracts/TestUSDT.sol` on **BSC Testnet**, with a funded
   test wallet (for example using Remix or Foundry). The constructor refuses
   networks other than 97. The deployer receives test tokens; its unrestricted
   `mint` method is intentionally for tests. This is not official USDT.
4. Copy `.env.example` to `.env` in the separate deployment directory. Set the
   deployed `TOKEN_CONTRACT`, `CHAIN_ID=97`, a suitable RPC URL, random API key
   (32+ characters), and testnet xpub. Run `docker compose up -d --build`.
5. Create an invoice, transfer test tokens to its address and poll the invoice.
   Repeat with partial, excess and late payments. No deposit to a mainnet
   address or mainnet USDT contract is needed.

The local integration test is automated. A public-testnet end-to-end test
requires faucet funding and a deployed test token and is **not claimed as run**.

## Mainnet configuration

Start with a separate, backed-up wallet and database. Set `CHAIN_ID=56`, a
trusted HTTPS RPC, and `TOKEN_CONTRACT=0x55d398326f99059ff775485246999027b3197955`.
This contract uses 18 decimals. On mainnet the API rejects any other contract.
The server validates the RPC chain, token decimals and finalized-block support
before accepting requests. Database identity is bound to xpub, chain and token
and cannot silently change on restart.

BNB's [API reference](https://docs.bnbchain.org/bnb-smart-chain/developers/json_rpc/bsc-api-list/)
describes finality. This service requires both finality and your configured
`CONFIRMATIONS` (default 15); it never falls back to crediting unfinalized blocks.
The RPC provider is a trusted data source. A provider outage leaves cursors
unchanged for retry.

## Automatic collection to your main wallet

![Optional gas funding and USDT collection sequence](docs/diagrams/sweep-flow.svg)

[Interactive collection sequence](docs/diagrams/sweep-flow.html) ·
[Architecture](#architecture)


Set the main BSC wallet **address** in `.env`. You keep its seed/private key in
your own wallet to spend funds later; receiving collections does not require
giving its key to this service. The signer needs the **deposit wallet seed**
matching `WALLET_XPUB` and a dedicated **gas funding wallet** key.

```text
Confirmed customer deposit
    -> child has enough BNB? -> send USDT to main wallet
    -> otherwise fund child from gas wallet
         -> wait for gas transaction finality + confirmations
         -> send USDT to main wallet
         -> verify the outgoing token Transfer log and finality
```

The main wallet may be managed in a wallet application or hardware wallet.
General payouts from it to arbitrary recipients are outside this receiver API.

Prepare files on the host (the commands print only the gas wallet address):

```sh
install -d -m 0700 secrets
install -o 10001 -g 10001 -m 0400 /secure/offline/deposit-wallet.seed secrets/deposit.seed
bin/cryptopayments gas-wallet-init secrets/gas.key
chown 10001:10001 secrets/gas.key
chmod 0400 secrets/gas.key
```

Fund the printed gas wallet with BNB **on the same network**. This balance pays
the gas funding transaction and the children's token-transfer gas. Without BNB
the worker waits and reports an error; it cannot create BNB or mark collection
complete. Use the gas wallet only for this worker so another sender cannot
consume its nonces.

Configure `.env`:

```dotenv
AUTO_SWEEP_ENABLED=true
MAIN_WALLET_ADDRESS=0xYourMainWalletAddress
SWEEP_SEED_FILE=/run/secrets/deposit.seed
GAS_PRIVATE_KEY_FILE=/run/secrets/gas.key
SWEEP_SEED_HOST_PATH=./secrets/deposit.seed
GAS_KEY_HOST_PATH=./secrets/gas.key
SWEEP_POLL_SECONDS=10
SWEEP_TOKEN_GAS_LIMIT=100000
SWEEP_MAX_GAS_PRICE_WEI=10000000000
SWEEP_MAX_TOPUP_WEI=2000000000000000
SWEEP_MIN_AMOUNT=0
```

```sh
docker compose --profile sweep up -d --build
```

`AUTO_SWEEP_ENABLED` authorizes the worker; the `sweep` Compose profile starts it.
Without the worker, the receiver remains watch-only. Its seed must match the
configured xpub; mismatched seeds and broadly readable key files are rejected.
Secrets are mounted only in `sweeper`, read-only, with no published worker ports.

`SWEEP_MIN_AMOUNT=0` collects every positive confirmed deposit, including partial,
overpaid, late and later additional deposits. Increase it to accumulate small
deposits before spending gas. A collection uses the confirmed incoming total
minus previously completed collections. Invoice `received_amount` and `is_paid`
remain historical payment facts even after the child's token balance reaches 0.
An invoice's expiry does not disable collection.

The token transaction reserves the configured gas limit at the RPC gas price.
Gas funding adds a 25% buffer and subtracts existing child BNB. The gas-price cap
and cumulative per-job top-up cap prevent unbounded funding. Unused BNB remains
on the child for later deposits. These limits are in **wei**, not USDT.

Read progress:

```sh
curl -H "Authorization: Bearer $API_KEY" \
  http://127.0.0.1:18090/v1/invoices/INVOICE_ID/sweeps
curl -H "Authorization: Bearer $API_KEY" \
  http://127.0.0.1:18090/v1/status
```

Jobs expose `queued`, `gas_pending`, `transfer_pending`, `completed` or `review`,
plus destination, amount, last error, and gas/token transaction hashes. No raw
signed transaction, seed or private key is returned. `/v1/status.sweeper` reports
the worker heartbeat, gas-wallet address and last error. Payment readiness and
sweep readiness are separate; `/readyz` still describes the payment scanner.

Jobs snapshot their main-wallet address when created. Changing the environment
does not redirect an already queued or signed transaction; new jobs use the new
address. A shared-volume lock enforces one signer per database. Jobs are processed
serially to protect gas-wallet nonces. An unconfirmed transaction is never
replaced with a newly signed transfer: its exact signed bytes are committed to
SQLite **before broadcast** and replayed after timeouts or restarts. `prepared`
transaction state includes awaiting acknowledgement or confirmation.

If a transaction reverts, its nonce is consumed unexpectedly, or a successful
receipt lacks the expected USDT transfer, the job enters `review` and automatic
sends stop for operator reconciliation. Missing BNB, RPC outages and gas-price
limits wait/retry without signing duplicate transfers. Stuck pending transactions
are reported and retained; automatic fee replacement and administrative
reconciliation endpoints are outside this version. Do not reset the database to
retry a transfer. Inspect the stored hashes on the configured chain first.

Run isolated signed-transfer tests after building the image and host binary:

```sh
docker compose build api
go build -o bin/cryptopayments .
python3 scripts/sweep_test.py
```

The test uses disposable volumes, fresh keys, a separate local Anvil node and
ports 18091/18546. It never funds a public-chain wallet or touches the receiver's
normal database. Test-only keys and containers are removed on completion.

## Webhooks

Set one operator-controlled `WEBHOOK_URL` and a 32+ character `WEBHOOK_SECRET`.
No callback URLs are accepted from invoice input. HTTPS is required on mainnet.

Events are `invoice.created` and `invoice.updated`, with an immutable event
`id`, `created_at`, and an invoice snapshot. Delivery is at least once.

Headers:

```text
X-CryptoPayments-Event-ID: evt_...
X-CryptoPayments-Timestamp: <unix seconds for this delivery attempt>
X-CryptoPayments-Signature: sha256=<hex HMAC-SHA256>
```

Signature input is the exact bytes `timestamp + "." + raw_request_body`.
Verify with the shared secret in constant time, reject delivery timestamps
outside a small clock tolerance (for example 5 minutes), and deduplicate by
event ID. Record fulfilment by invoice ID as well. Reply 2xx only after durably
accepting the event. Retries keep the same event ID/body with a fresh delivery
timestamp, back off from 5 seconds to 1 hour and continue until acknowledged.
Redirects are not followed. A failed earlier event blocks newer events for the
same invoice, while other invoices can continue.

## Storage and operational scope

SQLite uses WAL and `synchronous=FULL`. Back it up with SQLite's online backup
API or while the service is stopped; copying only the main file during writes
can lose WAL state. Keep the offline seed and database backups separate from Git.
All historical addresses remain monitored; this first version processes up to
100 invoices per tick and 500 blocks per invoice. Large deployments will need
batched address filters/indexing and operational load testing. Run one API and
one dispatcher per database; horizontal workers are outside this first version.

## Development checks

```sh
go test -race -count=1 ./...
go vet ./...
python3 scripts/local_test.py test
```

Recompile the checked-in test contract artifacts if editing Solidity:

```sh
npx --yes solc@0.8.30 --bin --abi -o contracts/build contracts/TestUSDT.sol
```

## Repository map

| Path | Responsibility |
| --- | --- |
| [`http.go`](http.go) | HTTP routes, Bearer authentication and invoice input validation |
| [`wallet.go`](wallet.go) | HD address derivation and offline wallet initialization |
| [`scanner.go`](scanner.go) / [`rpc.go`](rpc.go) | Finalized-range scanning and JSON-RPC validation |
| [`store.go`](store.go) / [`amount.go`](amount.go) | SQLite transactions, payment state and exact amounts |
| [`webhook.go`](webhook.go) | HMAC signatures, ordered outbox delivery and retry/backoff |
| [`sweeper.go`](sweeper.go) / [`sweep_config.go`](sweep_config.go) | Isolated signer, BNB gas funding, token collection and key-file validation |
| [`sweep_store.go`](sweep_store.go) | Collection jobs, signed transaction ledger and worker heartbeat |
| [`scripts/sweep_test.py`](scripts/sweep_test.py) | Disposable signed-transfer, gas-funding and restart integration tests |
| [`openapi.yaml`](openapi.yaml) | Machine-readable API contract |
| [`scripts/local_test.py`](scripts/local_test.py) | Isolated Anvil setup and transfer integration tests |
| [`contracts/`](contracts/) | Test-only token source and compiled artifacts |
| [`docs/diagrams/`](docs/diagrams/) | Archify diagrams, SVG previews and editable JSON sources |

The architecture and flow diagrams describe both the watch-only receiver and
the optional signer at the revision pinned in their JSON. Local signed-transfer
tests passed; the diagrams do not imply a public-network deployment.
