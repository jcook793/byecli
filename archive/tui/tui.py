"""EBAY-TRACKER TTY — the terminal-native face of the tracker.

Run with: .venv/bin/python tui.py
Keys: enter row detail · s sync · c edit cost · t phosphor · r reload · q quit
Click a column header to sort; same column flips direction.
"""
import webbrowser
from datetime import datetime

from rich.table import Table as RichTable
from rich.text import Text
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal
from textual.screen import ModalScreen
from textual.theme import Theme
from textual.widgets import DataTable, Footer, Input, Label, Static

import ebay_sync
import tracker_core as core

GREEN = Theme(
    name="phosphor-green",
    primary="#3dff8b", secondary="#63b184", accent="#6ab6ff",
    foreground="#a4ecc2", background="#070b08", surface="#0c120d",
    panel="#0c120d", success="#3dff8b", warning="#d7a04d", error="#ff5c57",
    dark=True,
)
AMBER = Theme(
    name="phosphor-amber",
    primary="#ffc24d", secondary="#bb9448", accent="#7fc4ff",
    foreground="#eec687", background="#0d0903", surface="#140e06",
    panel="#140e06", success="#3dff8b", warning="#d7a04d", error="#ff5c57",
    dark=True,
)

# semantic colors stay fixed across phosphors, same as the web face
C_GREEN, C_RED, C_BLUE = "#3dff8b", "#ff5c57", "#6ab6ff"
C_MUTED, C_TAPE, C_FLIP = "#63b184", "#d7a04d", "#c792ea"

COLUMNS = ("ITEM", "TYPE", "ENDS", "COST", "SALE", "SHPCHG", "SHPCST", "FEES", "NET$", "NET%")
NUMERIC_COLS = {3, 4, 5, 6, 7, 8, 9}
ITEM_WIDTH = 34  # floor for the name column; it stretches with the terminal


def sort_value(it, col):
    """Sort key per column, mirroring the web face. None = blank (sinks)."""
    sold = it["status"] == "sold"
    if col == 0:
        return it["name"].lower()
    if col == 1:
        return it["source"]
    if col == 2:  # ends: active by end time, sold grouped after
        return ("z" + it["date_sold"]) if sold else (it["listing_end"] or "9999")
    if col == 3:
        return it["cost_basis"]
    if col == 4:
        return it["sale_price"] if sold else it["listing_price"]
    if col == 5:
        return it["shipping_charged"] if sold else None
    if col == 6:
        return it["label_cost"] if sold else None
    if col == 7:
        return it["ebay_fees"] if sold else it["fees_est"]
    if col == 8:
        return it["net_profit"] if it["net_profit"] is not None else it["net_est"]
    return it["roi"]


def header_text(col, label):
    """Column header; money columns right-align to sit over their values."""
    return Text(label, justify="right" if col >= 3 else "left")


def cells(it):
    """Render one item as a row of Rich Text cells."""
    sold = it["status"] == "sold"

    def m(value, style=""):
        return Text(core.money(value), style=style, justify="right")

    def est(value, style=C_MUTED):
        return Text("~" + core.money(value), style=style, justify="right")

    name = Text(it["name"], no_wrap=True, overflow="ellipsis")
    src = Text("FLIP", style=C_FLIP) if it["source"] == "flip" else Text("BYE", style=C_TAPE)
    if sold:
        ends = Text(f"[{it['sold_label'].upper()}]", style=C_GREEN)
    elif it["ends_label"]:
        ends = Text(f"[{it['ends_label'].upper()}]", style=C_BLUE)
    else:
        ends = Text(f"[{it['status'].upper()}]", style=C_MUTED)
    cost = (Text("—", style=C_MUTED, justify="right") if it["source"] == "declutter"
            else m(it["cost_basis"]))
    sale = m(it["sale_price"]) if sold else (
        est(it["listing_price"]) if it["listing_price"] else m(None))
    ship_chg = m(it["shipping_charged"] if sold else None)
    ship_cost = m(it["label_cost"] if sold else None)
    fees = m(it["ebay_fees"]) if sold else (
        est(it["fees_est"]) if it["fees_est"] else m(None))
    if it["net_profit"] is not None:
        net = m(it["net_profit"], style=C_GREEN if it["net_profit"] >= 0 else C_RED)
    elif it["net_est"] is not None:
        net = est(it["net_est"], style=C_MUTED if it["net_est"] >= 0 else C_RED)
    else:
        net = m(None)
    if it["roi"] is None:  # BYE items: no cost basis, no meaningful return
        net_pct = Text("—", style=C_MUTED, justify="right")
    else:
        pct = f"{it['roi']:.0%}" if sold else f"~{it['roi']:.0%}"
        net_pct = Text(pct, justify="right",
                       style=(C_GREEN if sold else C_MUTED) if it["roi"] >= 0 else C_RED)
    return name, src, ends, cost, sale, ship_chg, ship_cost, fees, net, net_pct


def full_stamp(iso):
    """'Sat Jul 11 11:15a' from an ISO UTC timestamp, in local time."""
    if not iso:
        return None
    try:
        dt = datetime.fromisoformat(iso.replace("Z", "+00:00")).astimezone()
    except ValueError:
        return None
    return f"{dt:%a %b} {dt.day} {dt.hour % 12 or 12}:{dt:%M}{'p' if dt.hour >= 12 else 'a'}"


class DetailModal(ModalScreen):
    """Auction detail overlay: listing facts on the left, money ledger right."""

    BINDINGS = [
        Binding("escape", "dismiss(None)", "close"),
        Binding("o", "open_ebay", "open on ebay"),
        Binding("c", "edit_cost", "edit cost"),
    ]

    def __init__(self, item):
        super().__init__()
        self.item = item

    def _body(self):
        it, meta = self.item, self.item.get("meta", {})
        sold = it["status"] == "sold"

        def line(txt, label, value, style=""):
            txt.append(f"{label:<10}", style=C_MUTED)
            txt.append(f"{value}\n", style=style)

        left = Text()
        line(left, "status", f"SOLD {it['sold_label']}" if sold
             else (f"ends {full_stamp(it['listing_end'])}" if it["listing_end"]
                   else it["status"].upper()),
             C_GREEN if sold else C_BLUE)
        line(left, "listed", it["date_listed"] or "—")
        if not sold:
            line(left, "asking", core.money(it["listing_price"]))
        line(left, "watchers", str(meta.get("watch_count", "— (sync)")))
        line(left, "bids", str(meta.get("bid_count", "— (sync)")))
        if sold:
            line(left, "buyer", meta.get("buyer", "— (sync)"))
            line(left, "ship to", meta.get("ship_to", "— (sync)"))
        line(left, "ebay #", it["ebay_item_id"] or "—")
        if it["ebay_order_id"]:
            line(left, "order", it["ebay_order_id"])

        right = Text()
        est = not sold

        def money_line(label, value, tilde=False, style=""):
            shown = "—" if value is None else (("~" if tilde else "") + core.money(value))
            line(right, label, shown, style)

        money_line("cost", it["cost_basis"])
        money_line("sale" if sold else "sale*", it["sale_price"] if sold else it["listing_price"], est)
        money_line("ship chrg", it["shipping_charged"] if sold else None)
        money_line("ship cost", it["label_cost"] if sold else None)
        money_line("fees", it["ebay_fees"] if sold else it["fees_est"], est)
        money_line("other", it["other_costs"] if sold else None)
        net = it["net_profit"] if sold else it["net_est"]
        money_line("NET", net, est,
                   (C_GREEN if net >= 0 else C_RED) if net is not None else "")

        grid = RichTable.grid(expand=True, padding=(0, 3))
        grid.add_column(ratio=5)
        grid.add_column(ratio=4)
        grid.add_row(left, right)
        return grid

    def compose(self) -> ComposeResult:
        from textual.containers import Vertical
        with Vertical(id="detail-box"):
            yield Label(self.item["name"], id="detail-title")
            yield Static(self._body(), id="detail-body")
            if self.item["notes"]:
                yield Label(f"notes: {self.item['notes']}", id="detail-notes")
            yield Label("[o] open on ebay · [c] edit cost · [esc] close", id="detail-hint")

    def action_open_ebay(self) -> None:
        if self.item["ebay_item_id"]:
            webbrowser.open(f"https://www.ebay.com/itm/{self.item['ebay_item_id']}")
        else:
            self.app.notify("no ebay item id on this row", severity="warning")

    def action_edit_cost(self) -> None:
        self.dismiss("cost")


class CostModal(ModalScreen):
    """Edit an item's cost basis; >0 marks it a FLIP."""

    BINDINGS = [Binding("escape", "dismiss(None)", "cancel")]

    def __init__(self, item):
        super().__init__()
        self.item = item

    def compose(self) -> ComposeResult:
        with Horizontal(id="cost-box"):
            yield Label(f"COST BASIS · {self.item['name'][:48]}", id="cost-title")
            yield Input(value=f"{self.item['cost_basis']:.2f}", id="cost-input",
                        type="number", placeholder="0.00")
            yield Label("enter to save · esc to cancel · >0 marks it a FLIP", id="cost-hint")

    def on_input_submitted(self, event: Input.Submitted) -> None:
        try:
            self.dismiss(float(event.value))
        except ValueError:
            self.dismiss(None)


class TrackerApp(App):
    TITLE = "EBAY-TRACKER"

    CSS = """
    #stats { height: auto; margin: 1 1 0 1; }
    .stat {
        border: solid $secondary;
        border-title-color: $secondary;
        padding: 0 1;
        margin-right: 1;
        width: 1fr;
        height: 3;
        content-align: center middle;
        text-style: bold;
    }
    #items { margin: 1; border: solid $secondary; border-title-color: $foreground; }
    CostModal, DetailModal { align: center middle; }
    #detail-box {
        width: 96; height: auto; max-height: 80%;
        border: solid $primary; background: $surface; padding: 1 2;
    }
    #detail-title { color: $primary; text-style: bold; margin-bottom: 1; }
    #detail-notes { color: $secondary; margin-top: 1; }
    #detail-hint { color: $secondary; margin-top: 1; }
    #cost-box {
        width: 80; height: 7; border: solid $primary; background: $surface;
        layout: vertical; padding: 1 2;
    }
    #cost-title { color: $primary; text-style: bold; }
    #cost-hint { color: $secondary; }
    """

    BINDINGS = [
        Binding("s", "sync", "sync ebay"),
        Binding("c", "edit_cost", "edit cost"),
        Binding("t", "toggle_phosphor", "phosphor"),
        Binding("r", "refresh", "reload"),
        Binding("q", "quit", "quit"),
    ]

    def __init__(self):
        super().__init__()
        self.conn = core.connect()
        self.items = []
        self.sort_col = 2   # default: ENDS
        self.sort_dir = 1

    def compose(self) -> ComposeResult:
        with Horizontal(id="stats"):
            for sid in ("net", "flips", "decl", "ship", "fees", "pending", "month"):
                yield Static("", id=f"stat-{sid}", classes="stat")
        table = DataTable(id="items")
        table.border_title = "ITEMS"
        yield table
        yield Footer()

    def on_mount(self) -> None:
        self.register_theme(GREEN)
        self.register_theme(AMBER)
        self.theme = "phosphor-green"
        table = self.query_one(DataTable)
        table.cursor_type = "row"
        # COST fits $9,999.99 — flips don't get pricier than that around here
        widths = {0: ITEM_WIDTH, 3: 9}
        for i, label in enumerate(COLUMNS):
            table.add_column(header_text(i, label), key=str(i), width=widths.get(i))
        self.refresh_data()

    # ── data ──────────────────────────────────────────────────────────

    def refresh_data(self) -> None:
        self.items = core.fetch_items(self.conn)
        self.rebuild_table()
        self.update_stats()
        self.call_after_refresh(self._fit_items)

    def on_resize(self, _event) -> None:
        self._fit_items()

    def _fit_items(self) -> None:
        """Stretch the ITEM column into whatever width the numbers leave over."""
        table = self.query_one(DataTable)
        avail = table.scrollable_content_region.width
        if not table.columns or not avail:
            return
        item = table.columns["0"]
        others = sum(col.get_render_width(table)
                     for key, col in table.columns.items() if key != "0")
        width = max(ITEM_WIDTH, avail - others - 2 * table.cell_padding)
        if item.auto_width or width != item.width:
            item.auto_width = False
            item.width = width
            table._clear_caches()
            table.refresh()

    def rebuild_table(self) -> None:
        table = self.query_one(DataTable)
        prev_key = None
        if table.row_count and table.cursor_row is not None:
            try:
                prev_key = table.coordinate_to_cell_key((table.cursor_row, 0)).row_key.value
            except Exception:
                prev_key = None
        table.clear()

        valued = [i for i in self.items if sort_value(i, self.sort_col) is not None]
        blanks = [i for i in self.items if sort_value(i, self.sort_col) is None]
        valued.sort(key=lambda i: sort_value(i, self.sort_col),
                    reverse=self.sort_dir < 0)
        ordered = valued + blanks  # blanks sink regardless of direction

        for it in ordered:
            table.add_row(*cells(it), key=str(it["id"]))

        arrow = " ▲" if self.sort_dir > 0 else " ▼"
        for i, label in enumerate(COLUMNS):
            table.columns[str(i)].label = header_text(
                i, label + (arrow if i == self.sort_col else ""))

        if prev_key is not None:
            try:
                row_index = table.get_row_index(prev_key)
                table.move_cursor(row=row_index)
            except Exception:
                pass

    def update_stats(self) -> None:
        s = core.summarize(self.items)

        def put(sid, title, text, style=""):
            w = self.query_one(f"#stat-{sid}", Static)
            w.border_title = title
            w.update(Text(text, style=style, justify="center"))

        pn = "bold " + (C_GREEN if s["net_profit"] >= 0 else C_RED)
        put("net", "NET PROFIT", core.money(s["net_profit"]), pn)
        put("flips", "FLIPS", f"{core.money(s['flip_profit'])} · {s['flip_roi']:.0%}",
            C_GREEN if s["flip_profit"] >= 0 else C_RED)
        put("decl", "DECLUTTER", core.money(s["declutter_profit"]),
            C_GREEN if s["declutter_profit"] >= 0 else C_RED)
        put("ship", "SHIP PROFIT", core.money(s["shipping_profit"]),
            C_GREEN if s["shipping_profit"] >= 0 else C_RED)
        put("fees", "EBAY FEES", f"{core.money(s['fees'])} · {s['fees_pct']:.1%}")
        put("pending", "PENDING", f"{core.money(s['pending_value'])} · {s['listed_count']} live")
        put("month", "THIS MONTH", f"{core.money(s['month_net'])} · {s['month_count']} sold",
            C_GREEN if s["month_net"] >= 0 else C_RED)

    # ── interactions ──────────────────────────────────────────────────

    def on_data_table_header_selected(self, event: DataTable.HeaderSelected) -> None:
        col = event.column_index
        self.sort_dir = -self.sort_dir if col == self.sort_col else 1
        self.sort_col = col
        self.rebuild_table()

    def current_item(self):
        table = self.query_one(DataTable)
        if not table.row_count or table.cursor_row is None:
            return None
        key = table.coordinate_to_cell_key((table.cursor_row, 0)).row_key.value
        return next((i for i in self.items if str(i["id"]) == key), None)

    def on_data_table_row_selected(self, event: DataTable.RowSelected) -> None:
        key = event.row_key.value
        item = next((i for i in self.items if str(i["id"]) == key), None)
        if item is None:
            return

        def done(result):
            if result == "cost":
                self.push_cost(item)

        self.push_screen(DetailModal(item), done)

    def push_cost(self, item) -> None:
        def save(cost):
            if cost is not None and core.set_cost(self.conn, item["id"], cost):
                self.refresh_data()
                self.notify(f"COST SET · {core.money(cost)}"
                            + (" · FLIP" if cost > 0 else " · DECL"))

        self.push_screen(CostModal(item), save)

    def action_edit_cost(self) -> None:
        item = self.current_item()
        if item is not None:
            self.push_cost(item)

    def action_refresh(self) -> None:
        self.refresh_data()
        self.notify("RELOADED")

    def action_toggle_phosphor(self) -> None:
        self.theme = ("phosphor-amber" if self.theme == "phosphor-green"
                      else "phosphor-green")

    def action_sync(self) -> None:
        self.notify("SYNCING…")
        self.run_worker(self._do_sync, thread=True, exclusive=True)

    def _do_sync(self) -> None:
        conn = core.connect()  # sqlite connections are not cross-thread
        try:
            stats = ebay_sync.run_sync(conn)
            msg = (f"SYNCED · {stats['new']} NEW · {stats['updated']} PRICE · "
                   f"{stats['sold']} SOLD · {stats['fees']} FEES · {stats['labels']} LABELS")
            self.call_from_thread(self.refresh_data)
            self.call_from_thread(self.notify, msg)
        except ebay_sync.SyncError as exc:
            self.call_from_thread(self.notify, str(exc), severity="error", timeout=10)
        finally:
            conn.close()


if __name__ == "__main__":
    TrackerApp().run()
