# byeCLI

Say goodbye to your stuff. Terminal tracker for eBay decluttering and flips:
shipping arbitrage (eBay-calculated shipping vs. purchased labels), exact
eBay fees, net profit.

```sh
cd byecli && go build -o byecli . && ./byecli
```

Single static binary (Bubble Tea + pure-Go SQLite, no cgo). Data lives in
`~/.local/share/byecli/byecli.db`, credentials in
`~/.config/byecli/config.json`; override with `$BYECLI_DB` / `--db` and
`$BYECLI_CONFIG`.

Keys: arrows/jk move · enter detail · `s` sync · `c` cost (>0 marks a FLIP,
0 a BYE) · `o` open on eBay (in detail) · `p` green/amber phosphor · `r`
reload · `1-0` or click a header to sort · `q` quit.

```sh
cd byecli && go test ./...   # canned-payload sync tests + UI render tests
```

## eBay sync

`s` pulls, for the last `sync_days` (default 90):

- **Active listings** (Trading API `GetMyeBaySelling`) — new rows appear
  automatically; asking price shows italic in SALE until sold.
- **Orders** (Sell Fulfillment API) — sale price, shipping charged, date sold.
- **Fees** (Finances API) — the exact final value fee per order.
- **eBay-bought labels** (Finances API `SHIPPING_LABEL`) — label cost, filled
  automatically when the label was bought through eBay (voided labels net out).

Manual fields — cost basis, source, other costs, notes — are never overwritten
by sync. Label cost is manual too (Pirate Ship has no API) unless the label was
bought through eBay; a hand-entered label cost always wins over the synced one.

### One-time credential setup

1. On [developer.ebay.com](https://developer.ebay.com), take the **"I do not
   persist eBay data"** exemption on the Marketplace Account Deletion page
   (personal-use app, own account only) — production keys stay disabled until
   this is done.
2. Under **Application Keys**, copy the production App ID → `client_id` and
   Cert ID → `client_secret` into `~/.config/byecli/config.json`.
3. On the **User Tokens** page → *Get a Token from eBay via Your Application*
   → choose **OAuth** (not Auth'n'Auth) and add a redirect URL config. The
   "auth accepted" URL only needs to be HTTPS, not real —
   `https://localhost/accepted` works. Note the generated **RuName**.
4. Run `python3 tools/get_refresh_token.py` (stdlib only): open the printed
   consent URL, sign in with your seller account, paste back the URL eBay
   redirects you to (it 404s; the code is in the address bar). It saves the
   **refresh token** (long-lived, ~18 months) into the config.

## Notes

- **Net profit** = sale + shipping charged − cost basis − label − eBay fees −
  other costs, and only counts once a date sold is set — unsold inventory
  never distorts totals.
- Fee estimates (`~`-marked) use FVF 13.6% + $0.40; the real fee arrives via
  sync. The FVF is charged on the full amount including shipping and tax, so
  estimates run low. Promoted-listing ad fees show up as separate
  transactions — put those in other costs.
- `archive/` holds the retired Python versions (Flask web dashboard and
  Textual TUI) this replaced. Same SQLite schema; frozen, not maintained.
  To revive the webapp: `python3 -m venv .venv && .venv/bin/pip install -r
  ../requirements.txt`, point `tracker_core.DB_PATH` at the byecli db, and
  put credentials where `ebay_sync.CONFIG_PATH` looks.
