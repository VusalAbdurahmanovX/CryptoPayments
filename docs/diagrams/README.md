# Architecture diagram

[View the architecture](architecture.svg) · [Edit the Archify source](architecture.json)

The single diagram shows the watch-only receiver, shared SQLite database,
optional isolated sweep signer, key mounts and main-wallet destination.
The API, scanner and webhook dispatcher share the receiver process.

Source revision: [`dfb5c9e`](https://github.com/VusalAbdurahmanovX/CryptoPayments/tree/dfb5c9e8fce1aea0efbddd0bfae43cf4ce5ff242).
The JSON includes repository source ranges. The SVG was exported from Archify;
source, artifact and browser checks passed and light/dark views were inspected.

To regenerate with [Archify](https://github.com/tt-a1i/archify), render the JSON
locally and export SVG from its viewer. Interactive HTML and browser receipts
belong in the ignored `.archify/` directory; commit only the SVG and JSON.
