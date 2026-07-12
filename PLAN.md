# byeCLI — the plan

From tracker to full auction processor: everything between "it sold" and
"it's on the truck" happens inside the TUI. One binary, one database,
no browser except when eBay forces it.

## 1. Track — done

Sync listings, orders, exact fees, and eBay-bought label costs into the
local ledger. Phosphor table, detail overlay, cost basis editing,
FLIP/BYE split, shipping arbitrage and net profit stats.

## 2. Distribution

goreleaser → GitHub Releases → Homebrew tap. XDG paths already make the
binary relocatable. VHS demo GIF for the README.

## 3. Settings and auth in the TUI

View and edit the config from inside the app — a settings overlay in the
same phosphor idiom (printer names, sync window, API keys) instead of
hand-editing `config.json`. The file stays the source of truth; the TUI
just becomes a friendlier way to touch it.

Same treatment for credentials: port tools/get_refresh_token.py to a
`byecli auth` flow so eBay token renewal (~18 months) doesn't need
Python, and EasyPost auth slots in alongside it when Ship arrives.
Secrets stay in `~/.config/byecli/config.json`, never in the repo.

## 4. Ship — the big sprint

The soup-to-nuts flow, driven from a "to ship" queue (sold, not yet
shipped, sorted by ship-by date):

1. **Queue** — sync captures the buyer's chosen service
   (`shippingServiceCode`/`shippingCarrierCode` from Fulfillment API,
   next to the ship-to we already store).
2. **Quote** — EasyPost rates for the package, defaulting to the service
   equivalent to what the buyer paid for (or better). Rate differences vs.
   what eBay charged the buyer: we eat them, the SHIP PROFIT bubble keeps
   score.
3. **Buy** — purchase the label through EasyPost; cost lands in the ledger
   as `label_source: "easypost"` (manual entry still wins, eBay-bought
   labels still detected for the stragglers).
4. **Print** — via `lpr`, one CUPS queue per document type: the 4×6
   label PDF to the thermal printer (Zebra ZP450), the byeCLI-branded
   packing slip to the laser printer (full page). Printer names in
   config. Package weight/dimensions entered at quote time, remembered
   per item.
5. **Confirm** — push tracking to eBay (`createShippingFulfillment`);
   buyer gets the shipped email, item flips to shipped in the queue.

Design note: EasyPost is the first shipping integration, not the design.
Quote and buy in core go through a small provider-agnostic interface
(address + parcel in; rates, label PDF, cost out) so another provider
can slot in later without touching the TUI or the ledger.

Needs: EasyPost account + `easypost_api_key` in the byecli config,
package weight/dims fields in the schema, a queue view + ship overlay
in the TUI.

Also in this sprint — ledger correctness, same sync layer:

- **Refunds/returns** — pick up `REFUND` transactions from the Finances
  feed (including the partial FVF credit-back) so a return adjusts net
  instead of silently overstating it.
- **Ad fees** — pick up `AD_FEE` transactions and fill other costs
  automatically, same manual-wins rules as labels; removes the last
  routinely hand-entered number.
- **Backup** — rotating `sqlite .backup` into
  `~/.local/share/byecli/backups/` before each sync, keep the last N.
  Manual fields are the only data eBay can't re-sync.

## 5. Sell-side automation

- **Relist** — auction ended unsold → one-key relist, optionally at a
  lower price.
- Maybe someday: Negotiation API: see watcher/interest counts,
  fire "send offer to interested buyers" from the table.
- Maybe someday: create listings from scratch, buyer messages.

## Non-goals

Strictly a local-only utility: no server modes, no telnet/SSH serving, no
network listeners of any kind. Instances on different machines are
completely independent (each syncs from eBay itself; manual fields live
only where they're entered). Also out: live/streaming updates
(last-synced is fine), the archived Python faces, multi-user anything,
persisting other people's data beyond what a personal seller account
sees.
