# CryptoPayments — Go USDT BEP20 receiver API

A standalone HTTP API for creating unique payment addresses and confirming USDT
payments on BNB Smart Chain. No website, admin panel or third-party payment gateway.

`Your application -> create invoice -> unique address -> token transfer -> confirmed status / signed webhook`

## What works

- One fresh HD address per invoice (`m/44'/60'/0'/0/<index>`).
- Idempotent order creation: repeat the same `order_id` and amount to get the same invoice.
- Exact integer arithmetic at 18 decimals; JSON amounts are **strings**.
- Partial payments accumulate; overpayments and late payments are explicit.
- Only the configured token contract and chain count as payment.
- Both configured confirmation depth **and** RPC `finalized` block are required.
- SQLite transactions, durable per-invoice scan cursors and unique `(tx_hash, log_index)` records.
- Durable signed webhook outbox with retry/backoff and event ordering per invoice.
- Watch-only API: xpub only. No seed, signing key, withdrawals or automatic sweep in the server.
- Mainnet (56), public BSC Testnet (97), and an isolated local EVM for repeatable tests.

This first version receives and verifies payments. Funds remain at the generated
addresses. Collecting them into a treasury requires a separate signing/sweep
service and BNB for gas. No real money has been used to validate this version.

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
