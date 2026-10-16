# CryptoPayments diagrams

These diagrams are generated with [Archify](https://github.com/tt-a1i/archify)
from the implementation at commit
[`e237f70`](https://github.com/VusalAbdurahmanovX/CryptoPayments/tree/e237f703d27d6e8deff866795ed56514ba5ab91b).

| Diagram | GitHub preview | Interactive viewer | Editable source |
| --- | --- | --- | --- |
| System architecture | [SVG](architecture.svg) | [HTML](architecture.html) | [JSON](architecture.json) |
| Invoice to confirmation | [SVG](payment-flow.svg) | [HTML](payment-flow.html) | [JSON](payment-flow.json) |

## View

GitHub displays the SVG previews directly in the main README. GitHub's file
viewer shows HTML source rather than running it. Download an HTML file and
open it in a browser to use the standalone interactive viewer. No server or
API credentials are required. The viewer supports light/dark themes,
search/focus, Live/Still motion and image/SVG export.

The SVG files come from the viewer's canonical dual-theme SVG export. They
contain the complete diagram, without temporary focus or motion overlays.

## Read the diagrams

- **Architecture:** components and data ownership. The API, scanner and
  dispatcher are roles inside one Go process, not separately deployed services.
- **Sequence:** a new invoice, an external token transfer, confirmed credit,
  polling and optional webhook delivery. Vertical spacing indicates order,
  not measured duration. Webhook delivery is asynchronous: the dispatcher can
  run between scan/poll steps and sends `invoice.created` as well as updates.
- SQLite holds invoice and transfer state; it does not hold deposited tokens.
  Funds remain at the derived on-chain addresses.
- The Go service keeps only the xpub. The offline seed and a future sweep
  service are outside the runtime shown here.
- Payment acceptance includes excess and late payments. Fulfilment policy,
  especially for late orders, belongs to the merchant application.

Every semantic component/participant includes source ranges in the JSON.
The pinned SSH origin uses local-only source links in the viewer.

## Regenerate

Install Archify separately and set `ARCHIFY_CLI` to its `bin/archify.mjs` path.
Run from the repository root, with Node.js and Chrome/Chromium available.
First set `meta.output` in `architecture.json` to
`docs/diagrams/architecture.html`, and in `payment-flow.json` to
`docs/diagrams/payment-flow.html`:

```sh
export ARCHIFY_CLI=/path/to/archify/bin/archify.mjs
# If Chrome is not discovered automatically:
# export ARCHIFY_CHROME=/path/to/chrome
node "$ARCHIFY_CLI" finalize architecture docs/diagrams/architecture.json docs/diagrams/architecture.html --repo-root . --quality showcase --out-dir .archify/architecture-rebuild --json
node "$ARCHIFY_CLI" finalize sequence docs/diagrams/payment-flow.json docs/diagrams/payment-flow.html --repo-root . --quality showcase --out-dir .archify/sequence-rebuild --json
```

The committed JSON preserves its original `.archify/` authoring path; the
`meta.output` change above prepares a rebuild into the published location.
Use a fresh evidence directory when rebuilding an existing artifact. When
code changes, update the pinned revision and recheck the cited source ranges.
Then open the generated HTML and choose **Export → SVG** to refresh the README
previews. Commit the HTML, SVG and JSON together.

Archify's source, artifact and browser gates passed for both diagrams. Light
and dark screenshots were also inspected. These checks validate documentation;
they are not public BSC Testnet or mainnet transaction tests.
