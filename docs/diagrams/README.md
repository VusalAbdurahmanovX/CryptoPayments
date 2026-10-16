# CryptoPayments diagrams

These diagrams are generated with [Archify](https://github.com/tt-a1i/archify)
from the implementation at commit
[`dfb5c9e`](https://github.com/VusalAbdurahmanovX/CryptoPayments/tree/dfb5c9e8fce1aea0efbddd0bfae43cf4ce5ff242).

| Diagram | GitHub preview | Interactive viewer | Editable source |
| --- | --- | --- | --- |
| System architecture | [SVG](architecture.svg) | [HTML](architecture.html) | [JSON](architecture.json) |
| Invoice to confirmation | [SVG](payment-flow.svg) | [HTML](payment-flow.html) | [JSON](payment-flow.json) |
| Automatic USDT collection | [SVG](sweep-flow.svg) | [HTML](sweep-flow.html) | [JSON](sweep-flow.json) |

## View

GitHub displays the SVG previews directly in the main README. GitHub's file
viewer shows HTML source rather than running it. Download an HTML file and
open it in a browser to use the standalone interactive viewer. No server or
API credentials are required. The viewer supports light/dark themes,
search/focus, Live/Still motion and image/SVG export.

The SVG files come from the viewer's canonical dual-theme SVG export. They
contain the complete diagram, without temporary focus or motion overlays.

## Read the diagrams

- **Architecture:** components and data ownership. HTTP, scanner and webhook
  dispatcher share the receiver process. The optional sweep signer runs in a
  separate container with read-only key mounts and a shared SQLite volume.
- **Sequence:** a new invoice, an external token transfer, confirmed credit,
  polling and optional webhook delivery. Vertical spacing indicates order,
  not measured duration. Webhook delivery is asynchronous: the dispatcher can
  run between scan/poll steps and sends `invoice.created` as well as updates.
- **Collection sequence:** the optional signer reads confirmed incoming funds,
  funds child BNB only when needed, persists signed bytes before broadcast,
  waits for finality and verifies the outgoing token transfer to the main wallet.
  The BNB funding steps are skipped when the child already has enough gas.
- SQLite holds payment and collection records, including signed transaction
  bytes; tokens live on-chain. Invoice totals remain historical after collection.
- Only the receiver is watch-only. The optional signer reads the deposit seed
  and dedicated gas-wallet key; the main wallet is a destination address only.
- Payment acceptance includes excess and late payments. Fulfilment policy,
  especially for late orders, belongs to the merchant application.

Every semantic component/participant includes source ranges in the JSON.
The pinned SSH origin uses local-only source links in the viewer.

## Regenerate

Install Archify separately and set `ARCHIFY_CLI` to its `bin/archify.mjs` path.
Run from the repository root, with Node.js and Chrome/Chromium available.
First set `meta.output` in `architecture.json` to
`docs/diagrams/architecture.html`, and in `payment-flow.json` to
`docs/diagrams/payment-flow.html`. Set the matching `meta.output` in
`sweep-flow.json` to `docs/diagrams/sweep-flow.html`:

```sh
export ARCHIFY_CLI=/path/to/archify/bin/archify.mjs
# If Chrome is not discovered automatically:
# export ARCHIFY_CHROME=/path/to/chrome
node "$ARCHIFY_CLI" finalize architecture docs/diagrams/architecture.json docs/diagrams/architecture.html --repo-root . --quality showcase --out-dir .archify/architecture-rebuild --json
node "$ARCHIFY_CLI" finalize sequence docs/diagrams/payment-flow.json docs/diagrams/payment-flow.html --repo-root . --quality showcase --out-dir .archify/sequence-rebuild --json
node "$ARCHIFY_CLI" finalize sequence docs/diagrams/sweep-flow.json docs/diagrams/sweep-flow.html --repo-root . --quality showcase --out-dir .archify/sweep-rebuild --json
```

The committed JSON preserves its original `.archify/` authoring path; the
`meta.output` change above prepares a rebuild into the published location.
Use a fresh evidence directory when rebuilding an existing artifact. When
code changes, update the pinned revision and recheck the cited source ranges.
Then open the generated HTML and choose **Export → SVG** to refresh the README
previews. Commit the HTML, SVG and JSON together.

Archify's source, artifact and browser gates passed for all three diagrams. Light
and dark screenshots were also inspected. These checks validate documentation;
they are not public BSC Testnet or mainnet transaction tests.
