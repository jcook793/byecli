"""Pull active listings, orders, and fees from the eBay Sell APIs into the local db.

Credentials live in ebay_config.json (see ebay_config.example.json). The fetch_*
functions talk to eBay; the apply_* functions write to the db and are pure enough
to test with canned payloads.
"""
import json
import xml.etree.ElementTree as ET
from datetime import datetime, timedelta, timezone
from pathlib import Path

import requests

CONFIG_PATH = Path(__file__).parent / "ebay_config.json"

HOSTS = {
    "production": {
        "auth": "https://api.ebay.com",
        "api": "https://api.ebay.com",
        "finances": "https://apiz.ebay.com",
        "trading": "https://api.ebay.com/ws/api.dll",
    },
    "sandbox": {
        "auth": "https://api.sandbox.ebay.com",
        "api": "https://api.sandbox.ebay.com",
        "finances": "https://apiz.sandbox.ebay.com",
        "trading": "https://api.sandbox.ebay.com/ws/api.dll",
    },
}

# scope URLs are the same strings for sandbox and production
SCOPES = " ".join([
    "https://api.ebay.com/oauth/api_scope",
    "https://api.ebay.com/oauth/api_scope/sell.fulfillment",
    "https://api.ebay.com/oauth/api_scope/sell.finances",
])

TRADING_NS = "urn:ebay:apis:eBLBaseComponents"
TIMEOUT = 30


class SyncError(Exception):
    """Anything that should surface to the user as a sync failure banner."""


def load_config():
    if not CONFIG_PATH.exists():
        raise SyncError(
            "No ebay_config.json found — copy ebay_config.example.json to "
            "ebay_config.json and fill in your keys."
        )
    cfg = json.loads(CONFIG_PATH.read_text())
    for key in ("client_id", "client_secret", "refresh_token"):
        if not cfg.get(key) or cfg[key].startswith("YOUR-"):
            raise SyncError(f"ebay_config.json is missing '{key}'.")
    cfg.setdefault("environment", "production")
    cfg.setdefault("sync_days", 90)
    if cfg["environment"] not in HOSTS:
        raise SyncError("environment must be 'production' or 'sandbox'.")
    return cfg


def get_access_token(cfg):
    hosts = HOSTS[cfg["environment"]]
    resp = requests.post(
        f"{hosts['auth']}/identity/v1/oauth2/token",
        auth=(cfg["client_id"], cfg["client_secret"]),
        data={
            "grant_type": "refresh_token",
            "refresh_token": cfg["refresh_token"],
            "scope": SCOPES,
        },
        timeout=TIMEOUT,
    )
    if resp.status_code != 200:
        raise SyncError(f"Token refresh failed ({resp.status_code}): {resp.text[:300]}")
    return resp.json()["access_token"]


# ── fetchers ─────────────────────────────────────────────────────────────

def fetch_active_listings(cfg, token):
    """Active listings via the Trading API (covers listings made on ebay.com)."""
    hosts = HOSTS[cfg["environment"]]
    listings, page = [], 1
    while page <= 10:  # 200/page; a personal account won't blow past this
        body = f"""<?xml version="1.0" encoding="utf-8"?>
<GetMyeBaySellingRequest xmlns="{TRADING_NS}">
  <DetailLevel>ReturnAll</DetailLevel>
  <ActiveList>
    <Include>true</Include>
    <Pagination>
      <EntriesPerPage>200</EntriesPerPage>
      <PageNumber>{page}</PageNumber>
    </Pagination>
  </ActiveList>
</GetMyeBaySellingRequest>"""
        resp = requests.post(
            hosts["trading"],
            data=body.encode(),
            headers={
                "X-EBAY-API-COMPATIBILITY-LEVEL": "1193",
                "X-EBAY-API-CALL-NAME": "GetMyeBaySelling",
                "X-EBAY-API-SITEID": "0",
                "X-EBAY-API-IAF-TOKEN": token,
                "Content-Type": "text/xml",
            },
            timeout=TIMEOUT,
        )
        resp.raise_for_status()
        root = ET.fromstring(resp.text)

        def find(el, path):
            return el.find("/".join(f"{{{TRADING_NS}}}{p}" for p in path.split("/")))

        ack = find(root, "Ack")
        if ack is not None and ack.text == "Failure":
            err = find(root, "Errors/LongMessage")
            raise SyncError(f"GetMyeBaySelling failed: {err.text if err is not None else resp.text[:300]}")

        active = find(root, "ActiveList")
        if active is None:
            break
        for item in active.iter(f"{{{TRADING_NS}}}Item"):
            item_id = find(item, "ItemID")
            title = find(item, "Title")
            price = find(item, "SellingStatus/CurrentPrice")
            start = find(item, "ListingDetails/StartTime")
            end = find(item, "ListingDetails/EndTime")
            watch = find(item, "WatchCount")
            bids = find(item, "SellingStatus/BidCount")
            if item_id is None or title is None:
                continue
            listings.append({
                "item_id": item_id.text,
                "title": title.text,
                "price": float(price.text) if price is not None else None,
                "start_date": start.text[:10] if start is not None and start.text else None,
                "end_time": end.text if end is not None else None,
                "watch_count": int(watch.text) if watch is not None else None,
                "bid_count": int(bids.text) if bids is not None else None,
            })
        more = find(root, "ActiveList/PaginationResult/TotalNumberOfPages")
        if more is None or page >= int(more.text):
            break
        page += 1
    return listings


def _paged_get(url, token, params):
    """GET with Bearer auth, following 'next' links; yields records."""
    headers = {"Authorization": f"Bearer {token}"}
    while url:
        resp = requests.get(url, headers=headers, params=params, timeout=TIMEOUT)
        if resp.status_code != 200:
            raise SyncError(f"{url.split('?')[0]} failed ({resp.status_code}): {resp.text[:300]}")
        data = resp.json()
        yield data
        url = data.get("next")
        params = None  # baked into the next link


def fetch_orders(cfg, token, since_iso):
    hosts = HOSTS[cfg["environment"]]
    orders = []
    for page in _paged_get(
        f"{hosts['api']}/sell/fulfillment/v1/order",
        token,
        {"filter": f"creationdate:[{since_iso}..]", "limit": "200"},
    ):
        orders.extend(page.get("orders", []))
    return orders


def fetch_fees(cfg, token, since_iso):
    """Map of orderId -> total eBay fees, from SALE transactions."""
    hosts = HOSTS[cfg["environment"]]
    fees = {}
    for page in _paged_get(
        f"{hosts['finances']}/sell/finances/v1/transaction",
        token,
        {"filter": f"transactionDate:[{since_iso}..],transactionType:{{SALE}}", "limit": "200"},
    ):
        for txn in page.get("transactions", []):
            order_id = txn.get("orderId")
            fee = (txn.get("totalFeeAmount") or {}).get("value")
            if order_id and fee is not None:
                fees[order_id] = fees.get(order_id, 0.0) + float(fee)
    return fees


def fetch_label_costs(cfg, token, since_iso):
    """Map of orderId -> net cost of labels bought through eBay.

    Labels bought elsewhere (Pirate Ship) never appear here — those stay
    manual. Voided labels come back as CREDIT transactions and net out.
    """
    hosts = HOSTS[cfg["environment"]]
    labels = {}
    for page in _paged_get(
        f"{hosts['finances']}/sell/finances/v1/transaction",
        token,
        {"filter": f"transactionDate:[{since_iso}..],transactionType:{{SHIPPING_LABEL}}",
         "limit": "200"},
    ):
        for txn in page.get("transactions", []):
            order_id = txn.get("orderId")
            amount = (txn.get("amount") or {}).get("value")
            if not order_id or amount is None:
                continue
            sign = -1.0 if txn.get("bookingEntry") == "CREDIT" else 1.0
            labels[order_id] = labels.get(order_id, 0.0) + sign * float(amount)
    return labels


# ── db writers (testable with canned payloads) ───────────────────────────

def apply_listings(db, listings):
    stats = {"new": 0, "updated": 0}
    for li in listings:
        meta = {k: li[k] for k in ("watch_count", "bid_count") if li.get(k) is not None}
        meta_json = json.dumps(meta) if meta else None
        row = db.execute(
            "SELECT id, listing_price FROM items WHERE ebay_item_id=?", (li["item_id"],)
        ).fetchone()
        if row:
            db.execute(
                "UPDATE items SET name=?, listing_price=?, "
                "listing_end=COALESCE(?, listing_end), "
                "date_listed=COALESCE(?, date_listed), "
                "ebay_meta=COALESCE(?, ebay_meta) WHERE id=?",
                (li["title"], li["price"], li.get("end_time"), li["start_date"],
                 meta_json, row["id"]),
            )
            if row["listing_price"] != li["price"]:
                stats["updated"] += 1
        else:
            db.execute(
                "INSERT INTO items (name, source, date_listed, listing_price, "
                "listing_end, ebay_item_id, ebay_meta) "
                "VALUES (?, 'declutter', ?, ?, ?, ?, ?)",
                (li["title"], li["start_date"], li["price"], li.get("end_time"),
                 li["item_id"], meta_json),
            )
            stats["new"] += 1
    return stats


def apply_orders(db, orders):
    stats = {"sold": 0}
    for order in orders:
        if (order.get("cancelStatus") or {}).get("cancelState") == "CANCELED":
            continue
        order_id = order["orderId"]
        order_ship = float((order.get("pricingSummary", {}).get("deliveryCost") or {}).get("value", 0))
        sold_date = (order.get("creationDate") or "")[:10] or None

        order_meta = {}
        buyer = (order.get("buyer") or {}).get("username")
        if buyer:
            order_meta["buyer"] = buyer
        fsi = order.get("fulfillmentStartInstructions") or []
        if fsi:
            addr = (((fsi[0].get("shippingStep") or {}).get("shipTo") or {})
                    .get("contactAddress") or {})
            place = ", ".join(x for x in (addr.get("city"), addr.get("stateOrProvince")) if x)
            if place:
                order_meta["ship_to"] = place

        for idx, li in enumerate(order.get("lineItems", [])):
            item_id = li.get("legacyItemId")
            if not item_id:
                continue
            sale = float((li.get("lineItemCost") or {}).get("value", 0))
            # per-line shipping when present; else order-level shipping on the first line
            li_ship = (li.get("deliveryCost") or {}).get("value")
            ship = float(li_ship) if li_ship is not None else (order_ship if idx == 0 else 0.0)

            row = db.execute(
                "SELECT id, ebay_meta FROM items WHERE ebay_item_id=?", (item_id,)
            ).fetchone()
            if row:
                try:
                    merged = {**json.loads(row["ebay_meta"] or "{}"), **order_meta}
                except ValueError:
                    merged = dict(order_meta)
                db.execute(
                    "UPDATE items SET date_sold=?, sale_price=?, shipping_charged=?, "
                    "ebay_order_id=?, ebay_meta=? WHERE id=?",
                    (sold_date, sale, ship, order_id,
                     json.dumps(merged) if merged else None, row["id"]),
                )
            else:
                db.execute(
                    "INSERT INTO items (name, source, date_sold, sale_price, "
                    "shipping_charged, ebay_item_id, ebay_order_id, ebay_meta) "
                    "VALUES (?, 'declutter', ?, ?, ?, ?, ?, ?)",
                    (li.get("title", f"eBay item {item_id}"), sold_date, sale, ship,
                     item_id, order_id, json.dumps(order_meta) if order_meta else None),
                )
            stats["sold"] += 1
    return stats


def apply_fees(db, fees):
    stats = {"fees": 0}
    for order_id, fee in fees.items():
        cur = db.execute("UPDATE items SET ebay_fees=? WHERE ebay_order_id=?", (fee, order_id))
        stats["fees"] += cur.rowcount
    return stats


def apply_labels(db, labels):
    """Fill label_cost for labels bought through eBay.

    Only writes when the field is empty or was set by a previous sync
    (meta label_source == 'ebay') — a hand-entered Pirate Ship number wins.
    """
    stats = {"labels": 0}
    for order_id, cost in labels.items():
        row = db.execute(
            "SELECT id, label_cost, ebay_meta FROM items WHERE ebay_order_id=?", (order_id,)
        ).fetchone()
        if not row:
            continue
        try:
            meta = json.loads(row["ebay_meta"] or "{}")
        except ValueError:
            meta = {}
        if row["label_cost"] and meta.get("label_source") != "ebay":
            continue  # manual entry, leave it alone
        cost = round(cost, 2)
        if row["label_cost"] == cost:
            continue
        meta["label_source"] = "ebay"
        db.execute(
            "UPDATE items SET label_cost=?, ebay_meta=? WHERE id=?",
            (cost, json.dumps(meta), row["id"]),
        )
        stats["labels"] += 1
    return stats


def run_sync(db):
    cfg = load_config()
    try:
        token = get_access_token(cfg)
        since = (datetime.now(timezone.utc) - timedelta(days=cfg["sync_days"]))
        since_iso = since.strftime("%Y-%m-%dT%H:%M:%S.000Z")
        listings = fetch_active_listings(cfg, token)
        orders = fetch_orders(cfg, token, since_iso)
        fees = fetch_fees(cfg, token, since_iso)
        labels = fetch_label_costs(cfg, token, since_iso)
    except requests.RequestException as exc:
        raise SyncError(f"Network error talking to eBay: {exc}") from exc

    stats = {}
    stats.update(apply_listings(db, listings))
    stats.update(apply_orders(db, orders))
    stats.update(apply_fees(db, fees))
    stats.update(apply_labels(db, labels))
    db.commit()
    return stats
