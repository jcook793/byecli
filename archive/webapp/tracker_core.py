"""Shared core for the eBay tracker: schema, money math, and db operations.

Both faces of the tracker — the Flask web app and the Textual TUI — sit on
this module and share the same SQLite file directly (WAL mode, so concurrent
readers are fine for a single-user setup).
"""
import json
import sqlite3
from datetime import date, datetime
from pathlib import Path

DB_PATH = Path(__file__).parent / "ebay.db"

# eBay final value fee, calibrated to real sales (13.6% tier + $0.40 per order,
# verified to the penny on order 19-14861-95485). Still runs low pre-sale
# because eBay also takes its cut of shipping and sales tax — hence the ~ in
# the UI. Actual fees replace the estimate at sync time.
FVF_RATE = 0.136
FVF_FIXED = 0.40

MONEY_FIELDS = ("cost_basis", "sale_price", "shipping_charged",
                "label_cost", "ebay_fees", "other_costs")

SCHEMA = """
CREATE TABLE IF NOT EXISTS items (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'flip',   -- 'flip' or 'declutter'
    cost_basis       REAL NOT NULL DEFAULT 0,
    date_listed      TEXT,                           -- ISO yyyy-mm-dd
    date_sold        TEXT,
    sale_price       REAL NOT NULL DEFAULT 0,
    shipping_charged REAL NOT NULL DEFAULT 0,
    label_cost       REAL NOT NULL DEFAULT 0,
    ebay_fees        REAL NOT NULL DEFAULT 0,
    other_costs      REAL NOT NULL DEFAULT 0,
    notes            TEXT NOT NULL DEFAULT '',
    listing_price    REAL,                           -- current asking price on eBay
    listing_end      TEXT,                           -- ISO UTC auction end time
    ebay_item_id     TEXT UNIQUE,
    ebay_order_id    TEXT,
    ebay_meta        TEXT                            -- JSON: watchers, bids, buyer…
);
"""


def connect(db_path=DB_PATH):
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript(SCHEMA)
    return conn


def money(value):
    if value is None:
        return "—"
    sign = "-" if value < 0 else ""
    return f"{sign}${abs(value):,.2f}"


def ends_label(iso):
    """'Tue 7:38p' for auction ends this week, 'Jul 14' further out."""
    if not iso:
        return None
    try:
        dt = datetime.fromisoformat(iso.replace("Z", "+00:00")).astimezone()
    except ValueError:
        return None
    days_out = (dt.date() - date.today()).days
    if days_out < 7:
        return f"{dt:%a} {dt.hour % 12 or 12}:{dt:%M}{'p' if dt.hour >= 12 else 'a'}"
    return f"{dt:%b} {dt.day}"


def enrich(row):
    """Add computed fields to a DB row."""
    it = dict(row)
    try:
        it["meta"] = json.loads(it.get("ebay_meta") or "{}")
    except ValueError:
        it["meta"] = {}
    sold = bool(it["date_sold"])
    it["status"] = "sold" if sold else ("listed" if it["date_listed"] else "not listed")
    it["ends_label"] = None if sold else ends_label(it["listing_end"])
    if sold:
        d = date.fromisoformat(it["date_sold"])
        it["sold_label"] = f"{d:%b} {d.day}"
    it["fees_est"] = (FVF_RATE * it["listing_price"] + FVF_FIXED
                      if not sold and it["listing_price"] else None)
    # shipping intentionally excluded: no real label cost yet, assume it's a wash
    it["net_est"] = (it["listing_price"] - it["cost_basis"] - it["fees_est"]
                     if it["fees_est"] is not None else None)
    it["shipping_profit"] = it["shipping_charged"] - it["label_cost"] if sold else None
    it["net_profit"] = (
        it["sale_price"] + it["shipping_charged"] - it["cost_basis"]
        - it["label_cost"] - it["ebay_fees"] - it["other_costs"]
    ) if sold else None
    # return on cost, flips only; estimated (from net_est) until sold
    net = it["net_profit"] if sold else it["net_est"]
    it["roi"] = (net / it["cost_basis"]
                 if it["source"] == "flip" and it["cost_basis"] > 0 and net is not None
                 else None)
    return it


def fetch_items(conn):
    rows = conn.execute(
        "SELECT * FROM items ORDER BY (date_sold IS NOT NULL), date_sold DESC, id DESC"
    ).fetchall()
    return [enrich(r) for r in rows]


def summarize(items):
    sold = [i for i in items if i["status"] == "sold"]
    flips = [i for i in sold if i["source"] == "flip"]
    gross = sum(i["sale_price"] + i["shipping_charged"] for i in sold)
    fees = sum(i["ebay_fees"] for i in sold)
    flip_cost = sum(i["cost_basis"] for i in flips)
    flip_profit = sum(i["net_profit"] for i in flips)
    best = max(sold, key=lambda i: i["net_profit"], default=None)
    this_month = date.today().strftime("%Y-%m")
    month_sold = [i for i in sold if i["date_sold"][:7] == this_month]
    return {
        "best": {"name": best["name"], "net": best["net_profit"]} if best else None,
        "month_net": sum(i["net_profit"] for i in month_sold),
        "month_count": len(month_sold),
        "sold_count": len(sold),
        "gross": gross,
        "fees": fees,
        "fees_pct": fees / gross if gross else 0,
        "shipping_profit": sum(i["shipping_profit"] for i in sold),
        "net_profit": sum(i["net_profit"] for i in sold),
        "flip_count": len(flips),
        "flip_profit": flip_profit,
        "flip_roi": flip_profit / flip_cost if flip_cost else 0,
        "declutter_profit": sum(i["net_profit"] for i in sold if i["source"] == "declutter"),
        "listed_count": sum(1 for i in items if i["status"] == "listed"),
        "inventory_cost": sum(i["cost_basis"] for i in items if i["status"] != "sold"),
        "pending_value": sum(i["listing_price"] or 0 for i in items if i["status"] != "sold"),
    }


def monthly(items):
    """Net profit and gross by month sold, oldest first."""
    buckets = {}
    for i in items:
        if i["status"] != "sold":
            continue
        key = i["date_sold"][:7]  # yyyy-mm
        b = buckets.setdefault(key, {"net": 0.0, "gross": 0.0, "count": 0})
        b["net"] += i["net_profit"]
        b["gross"] += i["sale_price"] + i["shipping_charged"]
        b["count"] += 1
    out = []
    for key in sorted(buckets):
        y, m = key.split("-")
        label = date(int(y), int(m), 1).strftime("%b %Y")
        out.append({"label": label, **buckets[key]})
    return out


def set_cost(conn, item_id, cost):
    """Inline cost edit; a real cost basis means it was bought to resell."""
    if cost is None or cost < 0:
        return False
    conn.execute(
        "UPDATE items SET cost_basis=?, source=? WHERE id=?",
        (cost, "flip" if cost > 0 else "declutter", item_id),
    )
    conn.commit()
    return True
