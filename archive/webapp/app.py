"""eBay sales tracker — Flask web face over tracker_core."""
import json
from datetime import date

from flask import Flask, g, redirect, render_template, request, url_for

import ebay_sync
# re-exports keep external users (tests, shells) working via `from app import ...`
from tracker_core import (  # noqa: F401
    DB_PATH, FVF_FIXED, FVF_RATE, MONEY_FIELDS, SCHEMA,
    connect, enrich, ends_label, fetch_items, money, monthly, set_cost, summarize,
)

app = Flask(__name__)


def get_db():
    if "db" not in g:
        g.db = connect()
    return g.db


@app.teardown_appcontext
def close_db(_exc):
    db = g.pop("db", None)
    if db is not None:
        db.close()


def net_amount(v):
    return (v["sale_price"] + v["shipping_charged"] - v["cost_basis"]
            - v["label_cost"] - v["ebay_fees"] - v["other_costs"])


def form_to_values(form):
    def money_field(field):
        raw = form.get(field, "").strip()
        return float(raw) if raw else 0.0

    def day(field):
        raw = form.get(field, "").strip()
        return raw or None

    return {
        "name": form.get("name", "").strip(),
        "source": form.get("source", "flip"),
        "date_listed": day("date_listed"),
        "date_sold": day("date_sold"),
        "notes": form.get("notes", "").strip(),
        **{f: money_field(f) for f in MONEY_FIELDS},
    }


@app.template_filter("money")
def money_filter(value):
    return money(value)


@app.route("/")
def index():
    items = fetch_items(get_db())
    return render_template(
        "index.html",
        items=items,
        summary=summarize(items),
        monthly_json=json.dumps(monthly(items)),
    )


@app.route("/add", methods=["GET", "POST"])
def add():
    if request.method == "POST":
        v = form_to_values(request.form)
        if v["name"]:
            get_db().execute(
                """INSERT INTO items (name, source, cost_basis, date_listed, date_sold,
                       sale_price, shipping_charged, label_cost, ebay_fees, other_costs, notes)
                   VALUES (:name, :source, :cost_basis, :date_listed, :date_sold,
                       :sale_price, :shipping_charged, :label_cost, :ebay_fees, :other_costs, :notes)""",
                v,
            )
            get_db().commit()
            if v["date_sold"]:
                return redirect(url_for("index", chaching=f"{net_amount(v):.2f}"))
        return redirect(url_for("index"))
    return render_template("form.html", item=None, today=date.today().isoformat())


@app.route("/edit/<int:item_id>", methods=["GET", "POST"])
def edit(item_id):
    db = get_db()
    if request.method == "POST":
        old = db.execute("SELECT date_sold FROM items WHERE id=?", (item_id,)).fetchone()
        v = form_to_values(request.form)
        if v["name"]:
            v["id"] = item_id
            db.execute(
                """UPDATE items SET name=:name, source=:source, cost_basis=:cost_basis,
                       date_listed=:date_listed, date_sold=:date_sold, sale_price=:sale_price,
                       shipping_charged=:shipping_charged, label_cost=:label_cost,
                       ebay_fees=:ebay_fees, other_costs=:other_costs, notes=:notes
                   WHERE id=:id""",
                v,
            )
            db.commit()
            if old and not old["date_sold"] and v["date_sold"]:
                return redirect(url_for("index", chaching=f"{net_amount(v):.2f}"))
        return redirect(url_for("index"))
    row = db.execute("SELECT * FROM items WHERE id=?", (item_id,)).fetchone()
    if row is None:
        return redirect(url_for("index"))
    return render_template("form.html", item=dict(row), today=date.today().isoformat())


@app.route("/cost/<int:item_id>", methods=["POST"], endpoint="set_cost")
def set_cost_route(item_id):
    try:
        cost = float(request.form.get("cost_basis", "").strip())
    except ValueError:
        cost = None
    set_cost(get_db(), item_id, cost)
    return redirect(url_for("index"))


@app.route("/sync", methods=["POST"])
def sync():
    try:
        stats = ebay_sync.run_sync(get_db())
        msg = (f"Synced with eBay: {stats['new']} new listing{'s' if stats['new'] != 1 else ''}, "
               f"{stats['updated']} price update{'s' if stats['updated'] != 1 else ''}, "
               f"{stats['sold']} sale{'s' if stats['sold'] != 1 else ''}, "
               f"fees on {stats['fees']}, labels on {stats['labels']}")
        return redirect(url_for("index", sync=msg))
    except ebay_sync.SyncError as exc:
        return redirect(url_for("index", sync_error=str(exc)))


@app.route("/delete/<int:item_id>", methods=["POST"])
def delete(item_id):
    db = get_db()
    db.execute("DELETE FROM items WHERE id=?", (item_id,))
    db.commit()
    return redirect(url_for("index"))


if __name__ == "__main__":
    # 5000 collides with macOS AirPlay Receiver
    app.run(debug=True, port=5001)
