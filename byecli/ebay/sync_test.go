// Port of the Python canned-payload suite (test_sync.py): the apply functions
// must behave identically to ebay_sync.py against the same shapes, especially
// the never-clobber rules for manual fields.
package ebay

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"byecli/core"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := core.Connect(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

func scalar[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestSyncLifecycle(t *testing.T) {
	db := testDB(t)

	// 1. first sync sees two active listings
	listings := []Listing{
		{ItemID: "111", Title: "Canon 50mm lens", Price: fp(120), StartDate: "2026-07-01"},
		{ItemID: "222", Title: "Old sweater", Price: fp(25), StartDate: "2026-07-03"},
	}
	st, err := ApplyListings(db, listings)
	if err != nil || st.New != 2 || st.Updated != 0 {
		t.Fatalf("first sync: %+v, %v", st, err)
	}

	// user marks one as a flip with cost basis (manual fields)
	db.Exec("UPDATE items SET source='flip', cost_basis=40 WHERE ebay_item_id='111'")

	// 2. re-sync: price drop on the lens; manual fields survive
	listings[0].Price = fp(110)
	st, _ = ApplyListings(db, listings)
	if st.New != 0 || st.Updated != 1 {
		t.Fatalf("re-sync: %+v", st)
	}
	if v := scalar[float64](t, db, "SELECT listing_price FROM items WHERE ebay_item_id='111'"); v != 110 {
		t.Fatalf("price not updated: %v", v)
	}
	if src := scalar[string](t, db, "SELECT source FROM items WHERE ebay_item_id='111'"); src != "flip" {
		t.Fatal("manual source clobbered!")
	}
	if cb := scalar[float64](t, db, "SELECT cost_basis FROM items WHERE ebay_item_id='111'"); cb != 40 {
		t.Fatal("manual cost basis clobbered!")
	}

	// 3. the lens sells, plus an order for a never-seen item; cancelled skipped
	orders := []Order{
		mkOrder("ORD-1", "2026-07-09T14:00:00.000Z", "NONE_REQUESTED", "12.00",
			line("111", "Canon 50mm lens", "110.00", "12.00")),
		mkOrder("ORD-2", "2026-07-08T10:00:00.000Z", "", "6.00",
			line("333", "Mystery gadget", "30.00", "")),
		mkOrder("ORD-3", "2026-07-08T11:00:00.000Z", "CANCELED", "",
			line("222", "Old sweater", "25.00", "")),
	}
	st, err = ApplyOrders(db, orders)
	if err != nil || st.Sold != 2 {
		t.Fatalf("orders: %+v, %v", st, err)
	}
	if d := scalar[string](t, db, "SELECT date_sold FROM items WHERE ebay_item_id='111'"); d != "2026-07-09" {
		t.Fatalf("date_sold: %v", d)
	}
	if v := scalar[float64](t, db, "SELECT shipping_charged FROM items WHERE ebay_item_id='111'"); v != 12 {
		t.Fatalf("shipping: %v", v)
	}
	if cb := scalar[float64](t, db, "SELECT cost_basis FROM items WHERE ebay_item_id='111'"); cb != 40 {
		t.Fatal("manual cost basis clobbered by order sync!")
	}
	if name := scalar[string](t, db, "SELECT name FROM items WHERE ebay_item_id='333'"); name != "Mystery gadget" {
		t.Fatalf("unseen item: %v", name)
	}
	if v := scalar[float64](t, db, "SELECT shipping_charged FROM items WHERE ebay_item_id='333'"); v != 6 {
		t.Fatalf("order-level shipping fallback: %v", v)
	}
	if n := scalar[int](t, db, "SELECT COUNT(*) FROM items WHERE ebay_item_id='222' AND date_sold IS NOT NULL"); n != 0 {
		t.Fatal("cancelled order marked item sold!")
	}

	// 4. fees land by order id
	st, _ = ApplyFees(db, map[string]float64{"ORD-1": 17.85, "ORD-2": 5.12, "ORD-X": 9.99})
	if st.Fees != 2 {
		t.Fatalf("fees: %+v", st)
	}

	// 4b. eBay-bought labels fill label_cost, but never beat a manual entry
	db.Exec("UPDATE items SET label_cost=9.99 WHERE ebay_order_id='ORD-2'") // Pirate Ship, manual
	st, _ = ApplyLabels(db, map[string]float64{"ORD-1": 13.94, "ORD-2": 4.05, "ORD-X": 5})
	if st.Labels != 1 {
		t.Fatalf("labels: %+v", st)
	}
	if v := scalar[float64](t, db, "SELECT label_cost FROM items WHERE ebay_order_id='ORD-1'"); v != 13.94 {
		t.Fatalf("label: %v", v)
	}
	if v := scalar[float64](t, db, "SELECT label_cost FROM items WHERE ebay_order_id='ORD-2'"); v != 9.99 {
		t.Fatal("manual label cost clobbered!")
	}
	var meta map[string]any
	json.Unmarshal([]byte(scalar[string](t, db, "SELECT ebay_meta FROM items WHERE ebay_order_id='ORD-1'")), &meta)
	if meta["label_source"] != "ebay" {
		t.Fatalf("label_source: %v", meta)
	}
	// ebay-sourced value may update (voided + rebought); unchanged is a no-op
	st, _ = ApplyLabels(db, map[string]float64{"ORD-1": 15.20})
	if st.Labels != 1 {
		t.Fatalf("relabel: %+v", st)
	}
	st, _ = ApplyLabels(db, map[string]float64{"ORD-1": 15.20})
	if st.Labels != 0 {
		t.Fatalf("no-op relabel: %+v", st)
	}
	db.Exec("UPDATE items SET label_cost=13.94 WHERE ebay_order_id='ORD-1'")

	// 5. net profit math end-to-end via core
	items, err := core.FetchItems(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.EbayItemID != nil && *it.EbayItemID == "111" {
			want := 110 + 12 - 40 - 13.94 - 17.85 - 0
			if it.NetProfit == nil || math.Abs(*it.NetProfit-want) > 1e-9 {
				t.Fatalf("net profit: %v want %v", it.NetProfit, want)
			}
			if it.ROI == nil || math.Abs(*it.ROI-want/40) > 1e-9 {
				t.Fatalf("roi: %v", it.ROI)
			}
		}
	}
}

func mkOrder(id, created, cancel, orderShip string, lines ...orderLine) Order {
	var o Order
	o.OrderID = id
	o.CreationDate = created
	o.CancelStatus.CancelState = cancel
	if orderShip != "" {
		o.PricingSummary.DeliveryCost = &amount{orderShip}
	}
	for _, l := range lines {
		o.LineItems = append(o.LineItems, l)
	}
	return o
}

type orderLine = struct {
	LegacyItemID string  `json:"legacyItemId"`
	Title        string  `json:"title"`
	LineItemCost *amount `json:"lineItemCost"`
	DeliveryCost *amount `json:"deliveryCost"`
}

func line(itemID, title, cost, ship string) orderLine {
	l := orderLine{LegacyItemID: itemID, Title: title, LineItemCost: &amount{cost}}
	if ship != "" {
		l.DeliveryCost = &amount{ship}
	}
	return l
}

const tradingXML = `<?xml version="1.0" encoding="UTF-8"?>
<GetMyeBaySellingResponse xmlns="urn:ebay:apis:eBLBaseComponents">
  <Ack>Success</Ack>
  <ActiveList>
    <ItemArray>
      <Item>
        <ItemID>444</ItemID>
        <Title>Vintage radio</Title>
        <SellingStatus><CurrentPrice currencyID="USD">55.00</CurrentPrice><BidCount>3</BidCount></SellingStatus>
        <ListingDetails><StartTime>2026-07-05T01:02:03.000Z</StartTime><EndTime>2026-07-12T01:02:03.000Z</EndTime></ListingDetails>
        <WatchCount>7</WatchCount>
      </Item>
    </ItemArray>
    <PaginationResult><TotalNumberOfPages>1</TotalNumberOfPages></PaginationResult>
  </ActiveList>
</GetMyeBaySellingResponse>`

func TestFetchAndMetaMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tradingXML))
	}))
	defer srv.Close()
	hosts["test"] = hostSet{trading: srv.URL, api: srv.URL, finances: srv.URL, auth: srv.URL}
	cfg := &Config{Ebay: core.EbayConfig{Environment: "test"}}

	parsed, err := FetchActiveListings(cfg, "fake-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 {
		t.Fatalf("parsed: %+v", parsed)
	}
	li := parsed[0]
	if li.ItemID != "444" || li.Title != "Vintage radio" || *li.Price != 55 ||
		li.StartDate != "2026-07-05" || li.EndTime != "2026-07-12T01:02:03.000Z" ||
		*li.WatchCount != 7 || *li.BidCount != 3 {
		t.Fatalf("parsed listing: %+v", li)
	}

	// end_time and meta flow through ApplyListings
	db := testDB(t)
	ApplyListings(db, parsed)
	if v := scalar[string](t, db, "SELECT listing_end FROM items WHERE ebay_item_id='444'"); v != "2026-07-12T01:02:03.000Z" {
		t.Fatalf("listing_end: %v", v)
	}
	var meta map[string]any
	json.Unmarshal([]byte(scalar[string](t, db, "SELECT ebay_meta FROM items WHERE ebay_item_id='444'")), &meta)
	if meta["watch_count"].(float64) != 7 || meta["bid_count"].(float64) != 3 {
		t.Fatalf("meta: %v", meta)
	}

	// buyer meta merges in from orders without clobbering listing meta
	o := mkOrder("ORD-9", "2026-07-10T12:00:00.000Z", "", "5.00",
		line("444", "Vintage radio", "55.00", ""))
	o.Buyer.Username = "radiofan99"
	o.FulfillmentStartInstructions = append(o.FulfillmentStartInstructions,
		struct {
			ShippingStep struct {
				ShipTo struct {
					ContactAddress struct {
						City            string `json:"city"`
						StateOrProvince string `json:"stateOrProvince"`
					} `json:"contactAddress"`
				} `json:"shipTo"`
			} `json:"shippingStep"`
		}{})
	o.FulfillmentStartInstructions[0].ShippingStep.ShipTo.ContactAddress.City = "Dayton"
	o.FulfillmentStartInstructions[0].ShippingStep.ShipTo.ContactAddress.StateOrProvince = "OH"
	if _, err := ApplyOrders(db, []Order{o}); err != nil {
		t.Fatal(err)
	}
	meta = nil
	json.Unmarshal([]byte(scalar[string](t, db, "SELECT ebay_meta FROM items WHERE ebay_item_id='444'")), &meta)
	if meta["watch_count"].(float64) != 7 || meta["bid_count"].(float64) != 3 ||
		meta["buyer"] != "radiofan99" || meta["ship_to"] != "Dayton, OH" {
		t.Fatalf("merged meta: %v", meta)
	}
}

func TestFetchLabelCostsNetsCredits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"transactions": [
			{"transactionType": "SHIPPING_LABEL", "orderId": "ORD-1", "bookingEntry": "DEBIT", "amount": {"value": "13.94"}},
			{"transactionType": "SHIPPING_LABEL", "orderId": "ORD-1", "bookingEntry": "CREDIT", "amount": {"value": "13.94"}},
			{"transactionType": "SHIPPING_LABEL", "orderId": "ORD-1", "bookingEntry": "DEBIT", "amount": {"value": "15.20"}},
			{"transactionType": "SHIPPING_LABEL", "bookingEntry": "DEBIT", "amount": {"value": "7.77"}}
		]}`))
	}))
	defer srv.Close()
	hosts["test"] = hostSet{finances: srv.URL}
	lc, err := FetchLabelCosts(&Config{Ebay: core.EbayConfig{Environment: "test"}}, "tok", "2026-01-01T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(lc) != 1 || math.Abs(lc["ORD-1"]-15.20) > 1e-9 {
		t.Fatalf("label costs: %v", lc)
	}
}
