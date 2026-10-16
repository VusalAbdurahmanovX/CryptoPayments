# CryptoPayments

Self-hosted Go API for receiving USDT BEP20 payments on BNB Smart Chain.

- Unique deposit address per invoice; idempotent order creation.
- Partial payments accumulate; confirmed total ≥ requested amount means paid.
- Watch-only API, SQLite persistence and signed webhooks.
- Optional isolated signer for BNB gas funding and collection to your main wallet.

![Architecture](docs/diagrams/architecture.svg)

## Quick start

Requires Docker Compose and Python 3. This starts an isolated local EVM with test tokens.

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.25-bookworm sh -c 'go build -o bin/cryptopayments .'
python3 scripts/local_test.py setup
python3 scripts/local_test.py test
```

API: `http://127.0.0.1:18090`. All `/v1/*` routes require `Authorization: Bearer <API_KEY>`.
Local EVM tests pass; public-network validation is pending.

## Documentation

[API contract](openapi.yaml) · [Configuration](.env.example) ·
[Usage & deployment](docs/guide.md) · [Automatic collection](docs/guide.md#automatic-collection-to-your-main-wallet)
