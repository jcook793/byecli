// Package ebay pulls active listings, orders, fees, and eBay-bought shipping
// labels into the local db. Port of ebay_sync.py: the fetch functions talk to
// eBay, the apply functions write to the db and are testable with canned
// payloads. Manual fields (cost basis, source, notes, other costs, and any
// hand-entered label cost) are never overwritten.
package ebay

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"byecli/core"
)

// SyncError is anything that should surface to the user as a sync failure.
type SyncError struct{ msg string }

func (e *SyncError) Error() string { return e.msg }

func errf(format string, args ...any) error {
	return &SyncError{fmt.Sprintf(format, args...)}
}

// Config is the shared core config; sync needs the eBay credential fields.
type Config = core.Config

type hostSet struct{ auth, api, finances, trading string }

var hosts = map[string]hostSet{
	"production": {
		auth:     "https://api.ebay.com",
		api:      "https://api.ebay.com",
		finances: "https://apiz.ebay.com",
		trading:  "https://api.ebay.com/ws/api.dll",
	},
	"sandbox": {
		auth:     "https://api.sandbox.ebay.com",
		api:      "https://api.sandbox.ebay.com",
		finances: "https://apiz.sandbox.ebay.com",
		trading:  "https://api.sandbox.ebay.com/ws/api.dll",
	},
}

// scope URLs are the same strings for sandbox and production
const scopes = "https://api.ebay.com/oauth/api_scope " +
	"https://api.ebay.com/oauth/api_scope/sell.fulfillment " +
	"https://api.ebay.com/oauth/api_scope/sell.finances"

var httpClient = &http.Client{Timeout: 30 * time.Second}

func LoadConfig() (*Config, error) {
	cfg, err := core.LoadConfig()
	if err != nil {
		return nil, errf("%v", err)
	}
	for name, v := range map[string]string{
		"client_id": cfg.Ebay.ClientID, "client_secret": cfg.Ebay.ClientSecret,
	} {
		if v == "" || strings.HasPrefix(v, "YOUR-") {
			return nil, errf("config is missing %q — add your eBay keys in "+
				"settings (s) or %s", name, core.ConfigPath())
		}
	}
	if cfg.Ebay.RefreshToken == "" {
		return nil, errf("no refresh token — authorize in settings (s then a)")
	}
	if cfg.Ebay.Environment == "" {
		cfg.Ebay.Environment = "production"
	}
	if _, ok := hosts[cfg.Ebay.Environment]; !ok {
		return nil, errf("environment must be 'production' or 'sandbox'")
	}
	if cfg.Ebay.SyncDays == 0 {
		cfg.Ebay.SyncDays = 90
	}
	return cfg, nil
}

func getAccessToken(cfg *Config) (string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cfg.Ebay.RefreshToken},
		"scope":         {scopes},
	}
	req, _ := http.NewRequest("POST",
		hosts[cfg.Ebay.Environment].auth+"/identity/v1/oauth2/token",
		strings.NewReader(form.Encode()))
	req.SetBasicAuth(cfg.Ebay.ClientID, cfg.Ebay.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", errf("network error refreshing token: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", errf("token refresh failed (%d): %s", resp.StatusCode, trim(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", errf("token response made no sense: %s", trim(body))
	}
	return tok.AccessToken, nil
}

func trim(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ── fetchers ─────────────────────────────────────────────────────────────

type Listing struct {
	ItemID     string
	Title      string
	Price      *float64
	StartDate  string // yyyy-mm-dd
	EndTime    string // full ISO
	WatchCount *int
	BidCount   *int
}

type tradingResp struct {
	Ack    string `xml:"Ack"`
	Errors struct {
		LongMessage string `xml:"LongMessage"`
	} `xml:"Errors"`
	ActiveList struct {
		Items []struct {
			ItemID        string `xml:"ItemID"`
			Title         string `xml:"Title"`
			SellingStatus struct {
				CurrentPrice string `xml:"CurrentPrice"`
				BidCount     string `xml:"BidCount"`
			} `xml:"SellingStatus"`
			ListingDetails struct {
				StartTime string `xml:"StartTime"`
				EndTime   string `xml:"EndTime"`
			} `xml:"ListingDetails"`
			WatchCount string `xml:"WatchCount"`
		} `xml:"ItemArray>Item"`
		Pagination struct {
			TotalNumberOfPages int `xml:"TotalNumberOfPages"`
		} `xml:"PaginationResult"`
	} `xml:"ActiveList"`
}

// FetchActiveListings uses the Trading API (covers listings made on ebay.com).
func FetchActiveListings(cfg *Config, token string) ([]Listing, error) {
	var listings []Listing
	for page := 1; page <= 10; page++ { // 200/page; personal account won't blow past this
		body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<GetMyeBaySellingRequest xmlns="urn:ebay:apis:eBLBaseComponents">
  <DetailLevel>ReturnAll</DetailLevel>
  <ActiveList>
    <Include>true</Include>
    <Pagination>
      <EntriesPerPage>200</EntriesPerPage>
      <PageNumber>%d</PageNumber>
    </Pagination>
  </ActiveList>
</GetMyeBaySellingRequest>`, page)
		req, _ := http.NewRequest("POST", hosts[cfg.Ebay.Environment].trading,
			strings.NewReader(body))
		req.Header.Set("X-EBAY-API-COMPATIBILITY-LEVEL", "1193")
		req.Header.Set("X-EBAY-API-CALL-NAME", "GetMyeBaySelling")
		req.Header.Set("X-EBAY-API-SITEID", "0")
		req.Header.Set("X-EBAY-API-IAF-TOKEN", token)
		req.Header.Set("Content-Type", "text/xml")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, errf("network error talking to eBay: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, errf("GetMyeBaySelling failed (%d): %s", resp.StatusCode, trim(raw))
		}
		var parsed tradingResp
		if err := xml.Unmarshal(raw, &parsed); err != nil {
			return nil, errf("GetMyeBaySelling returned unparseable XML: %v", err)
		}
		if parsed.Ack == "Failure" {
			msg := parsed.Errors.LongMessage
			if msg == "" {
				msg = trim(raw)
			}
			return nil, errf("GetMyeBaySelling failed: %s", msg)
		}
		for _, x := range parsed.ActiveList.Items {
			if x.ItemID == "" || x.Title == "" {
				continue
			}
			li := Listing{ItemID: x.ItemID, Title: x.Title, EndTime: x.ListingDetails.EndTime}
			if p, err := strconv.ParseFloat(x.SellingStatus.CurrentPrice, 64); err == nil {
				li.Price = &p
			}
			if len(x.ListingDetails.StartTime) >= 10 {
				li.StartDate = x.ListingDetails.StartTime[:10]
			}
			if n, err := strconv.Atoi(x.WatchCount); err == nil {
				li.WatchCount = &n
			}
			if n, err := strconv.Atoi(x.SellingStatus.BidCount); err == nil {
				li.BidCount = &n
			}
			listings = append(listings, li)
		}
		if page >= parsed.ActiveList.Pagination.TotalNumberOfPages {
			break
		}
	}
	return listings, nil
}

type amount struct {
	Value string `json:"value"`
}

func (a *amount) float() (float64, bool) {
	if a == nil || a.Value == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(a.Value, 64)
	return v, err == nil
}

type Order struct {
	OrderID      string `json:"orderId"`
	CreationDate string `json:"creationDate"`
	CancelStatus struct {
		CancelState string `json:"cancelState"`
	} `json:"cancelStatus"`
	PricingSummary struct {
		DeliveryCost *amount `json:"deliveryCost"`
	} `json:"pricingSummary"`
	Buyer struct {
		Username string `json:"username"`
	} `json:"buyer"`
	FulfillmentStartInstructions []fulfillmentInstruction `json:"fulfillmentStartInstructions"`
	LineItems []struct {
		LegacyItemID string  `json:"legacyItemId"`
		Title        string  `json:"title"`
		LineItemCost *amount `json:"lineItemCost"`
		DeliveryCost *amount `json:"deliveryCost"`
	} `json:"lineItems"`
}

// fulfillmentInstruction carries what the ship flow needs: the service the
// buyer paid for (the EasyPost quote has to match or beat it) and the full
// street-level ship-to for the label.
type fulfillmentInstruction struct {
	ShippingStep struct {
		ShippingCarrierCode string `json:"shippingCarrierCode"`
		ShippingServiceCode string `json:"shippingServiceCode"`
		ShipTo              struct {
			FullName       string `json:"fullName"`
			ContactAddress struct {
				AddressLine1    string `json:"addressLine1"`
				AddressLine2    string `json:"addressLine2"`
				City            string `json:"city"`
				StateOrProvince string `json:"stateOrProvince"`
				PostalCode      string `json:"postalCode"`
				CountryCode     string `json:"countryCode"`
			} `json:"contactAddress"`
		} `json:"shipTo"`
	} `json:"shippingStep"`
}

// pagedGet follows 'next' links, feeding each page of JSON to handle.
func pagedGet(rawURL, token string, params url.Values, handle func([]byte) (string, error)) error {
	u := rawURL
	if params != nil {
		u += "?" + params.Encode()
	}
	for u != "" {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			return errf("network error talking to eBay: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return errf("%s failed (%d): %s", strings.Split(u, "?")[0],
				resp.StatusCode, trim(body))
		}
		next, err := handle(body)
		if err != nil {
			return err
		}
		u = next // params are baked into the next link
	}
	return nil
}

func FetchOrders(cfg *Config, token, sinceISO string) ([]Order, error) {
	var orders []Order
	err := pagedGet(hosts[cfg.Ebay.Environment].api+"/sell/fulfillment/v1/order", token,
		url.Values{"filter": {"creationdate:[" + sinceISO + "..]"}, "limit": {"200"}},
		func(body []byte) (string, error) {
			var page struct {
				Orders []Order `json:"orders"`
				Next   string  `json:"next"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return "", errf("order response made no sense: %v", err)
			}
			orders = append(orders, page.Orders...)
			return page.Next, nil
		})
	return orders, err
}

type txnPage struct {
	Transactions []struct {
		OrderID         string  `json:"orderId"`
		TransactionType string  `json:"transactionType"`
		BookingEntry    string  `json:"bookingEntry"`
		Amount          *amount `json:"amount"`
		TotalFeeAmount  *amount `json:"totalFeeAmount"`
	} `json:"transactions"`
	Next string `json:"next"`
}

func fetchTransactions(cfg *Config, token, sinceISO, txnType string,
	visit func(t txnPage)) error {
	return pagedGet(hosts[cfg.Ebay.Environment].finances+"/sell/finances/v1/transaction",
		token, url.Values{
			"filter": {"transactionDate:[" + sinceISO + "..],transactionType:{" + txnType + "}"},
			"limit":  {"200"},
		},
		func(body []byte) (string, error) {
			var page txnPage
			if err := json.Unmarshal(body, &page); err != nil {
				return "", errf("transaction response made no sense: %v", err)
			}
			visit(page)
			return page.Next, nil
		})
}

// FetchFees returns orderId -> total eBay fees, from SALE transactions.
func FetchFees(cfg *Config, token, sinceISO string) (map[string]float64, error) {
	fees := map[string]float64{}
	err := fetchTransactions(cfg, token, sinceISO, "SALE",
		func(page txnPage) {
			for _, t := range page.Transactions {
				if v, ok := t.TotalFeeAmount.float(); ok && t.OrderID != "" {
					fees[t.OrderID] += v
				}
			}
		})
	return fees, err
}

// FetchLabelCosts returns orderId -> net cost of labels bought through eBay.
// Labels bought elsewhere (Pirate Ship) never appear here — those stay
// manual. Voided labels come back as CREDIT transactions and net out.
func FetchLabelCosts(cfg *Config, token, sinceISO string) (map[string]float64, error) {
	labels := map[string]float64{}
	err := fetchTransactions(cfg, token, sinceISO, "SHIPPING_LABEL",
		func(page txnPage) {
			for _, t := range page.Transactions {
				v, ok := t.Amount.float()
				if !ok || t.OrderID == "" {
					continue
				}
				if t.BookingEntry == "CREDIT" {
					v = -v
				}
				labels[t.OrderID] += v
			}
		})
	return labels, err
}

// ── db writers (testable with canned payloads) ───────────────────────────

type Stats struct{ New, Updated, Sold, Fees, Labels int }

func metaJSON(m map[string]any) *string {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	s := string(b)
	return &s
}

func ApplyListings(db *sql.DB, listings []Listing) (Stats, error) {
	var st Stats
	for _, li := range listings {
		meta := map[string]any{}
		if li.WatchCount != nil {
			meta["watch_count"] = *li.WatchCount
		}
		if li.BidCount != nil {
			meta["bid_count"] = *li.BidCount
		}
		var startDate, endTime *string
		if li.StartDate != "" {
			startDate = &li.StartDate
		}
		if li.EndTime != "" {
			endTime = &li.EndTime
		}
		var id int64
		var oldPrice *float64
		err := db.QueryRow("SELECT id, listing_price FROM items WHERE ebay_item_id=?",
			li.ItemID).Scan(&id, &oldPrice)
		switch {
		case err == sql.ErrNoRows:
			if _, err := db.Exec(`INSERT INTO items (name, source, date_listed,
				listing_price, listing_end, ebay_item_id, ebay_meta)
				VALUES (?, 'declutter', ?, ?, ?, ?, ?)`,
				li.Title, startDate, li.Price, endTime, li.ItemID,
				metaJSON(meta)); err != nil {
				return st, err
			}
			st.New++
		case err != nil:
			return st, err
		default:
			if _, err := db.Exec(`UPDATE items SET name=?, listing_price=?,
				listing_end=COALESCE(?, listing_end),
				date_listed=COALESCE(?, date_listed),
				ebay_meta=COALESCE(?, ebay_meta) WHERE id=?`,
				li.Title, li.Price, endTime, startDate, metaJSON(meta), id); err != nil {
				return st, err
			}
			if !floatPtrEq(oldPrice, li.Price) {
				st.Updated++
			}
		}
	}
	return st, nil
}

func floatPtrEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ApplyOrders(db *sql.DB, orders []Order) (Stats, error) {
	var st Stats
	for _, order := range orders {
		if order.CancelStatus.CancelState == "CANCELED" {
			continue
		}
		orderShip, _ := order.PricingSummary.DeliveryCost.float()
		var soldDate *string
		if len(order.CreationDate) >= 10 {
			d := order.CreationDate[:10]
			soldDate = &d
		}

		orderMeta := map[string]any{}
		if order.Buyer.Username != "" {
			orderMeta["buyer"] = order.Buyer.Username
		}
		if len(order.FulfillmentStartInstructions) > 0 {
			step := order.FulfillmentStartInstructions[0].ShippingStep
			addr := step.ShipTo.ContactAddress
			var parts []string
			for _, p := range []string{addr.City, addr.StateOrProvince} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			if len(parts) > 0 {
				orderMeta["ship_to"] = strings.Join(parts, ", ")
			}
			if step.ShippingServiceCode != "" {
				orderMeta["ship_service"] = step.ShippingServiceCode
			}
			if step.ShippingCarrierCode != "" {
				orderMeta["ship_carrier"] = step.ShippingCarrierCode
			}
			// street-level ship-to, kept only while eBay still returns it
			// (addresses age out of the API): the label needs all of it
			if addr.AddressLine1 != "" {
				full := map[string]string{
					"name": step.ShipTo.FullName, "street1": addr.AddressLine1,
					"street2": addr.AddressLine2, "city": addr.City,
					"state": addr.StateOrProvince, "zip": addr.PostalCode,
					"country": addr.CountryCode,
				}
				for k, v := range full {
					if v == "" {
						delete(full, k)
					}
				}
				orderMeta["ship_to_full"] = full
			}
		}

		for idx, li := range order.LineItems {
			if li.LegacyItemID == "" {
				continue
			}
			sale, _ := li.LineItemCost.float()
			// per-line shipping when present; else order-level shipping on the first line
			ship := 0.0
			if v, ok := li.DeliveryCost.float(); ok {
				ship = v
			} else if idx == 0 {
				ship = orderShip
			}

			var id int64
			var oldMeta *string
			err := db.QueryRow("SELECT id, ebay_meta FROM items WHERE ebay_item_id=?",
				li.LegacyItemID).Scan(&id, &oldMeta)
			switch {
			case err == sql.ErrNoRows:
				name := li.Title
				if name == "" {
					name = "eBay item " + li.LegacyItemID
				}
				if _, err := db.Exec(`INSERT INTO items (name, source, date_sold,
					sale_price, shipping_charged, ebay_item_id, ebay_order_id, ebay_meta)
					VALUES (?, 'declutter', ?, ?, ?, ?, ?, ?)`,
					name, soldDate, sale, ship, li.LegacyItemID, order.OrderID,
					metaJSON(orderMeta)); err != nil {
					return st, err
				}
			case err != nil:
				return st, err
			default:
				merged := map[string]any{}
				if oldMeta != nil {
					_ = json.Unmarshal([]byte(*oldMeta), &merged)
				}
				for k, v := range orderMeta {
					merged[k] = v
				}
				if _, err := db.Exec(`UPDATE items SET date_sold=?, sale_price=?,
					shipping_charged=?, ebay_order_id=?, ebay_meta=? WHERE id=?`,
					soldDate, sale, ship, order.OrderID, metaJSON(merged), id); err != nil {
					return st, err
				}
			}
			st.Sold++
		}
	}
	return st, nil
}

func ApplyFees(db *sql.DB, fees map[string]float64) (Stats, error) {
	var st Stats
	for orderID, fee := range fees {
		res, err := db.Exec("UPDATE items SET ebay_fees=? WHERE ebay_order_id=?",
			fee, orderID)
		if err != nil {
			return st, err
		}
		n, _ := res.RowsAffected()
		st.Fees += int(n)
	}
	return st, nil
}

// ApplyLabels fills label_cost for labels bought through eBay. Only writes
// when the field is empty or was set by a previous sync (meta label_source ==
// "ebay") — a hand-entered Pirate Ship number wins.
func ApplyLabels(db *sql.DB, labels map[string]float64) (Stats, error) {
	var st Stats
	for orderID, cost := range labels {
		var id int64
		var labelCost float64
		var rawMeta *string
		err := db.QueryRow(
			"SELECT id, label_cost, ebay_meta FROM items WHERE ebay_order_id=?",
			orderID).Scan(&id, &labelCost, &rawMeta)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return st, err
		}
		meta := map[string]any{}
		if rawMeta != nil {
			_ = json.Unmarshal([]byte(*rawMeta), &meta)
		}
		if labelCost != 0 && meta["label_source"] != "ebay" {
			continue // manual entry, leave it alone
		}
		cost = math.Round(cost*100) / 100
		if labelCost == cost {
			continue
		}
		meta["label_source"] = "ebay"
		if _, err := db.Exec("UPDATE items SET label_cost=?, ebay_meta=? WHERE id=?",
			cost, metaJSON(meta), id); err != nil {
			return st, err
		}
		st.Labels++
	}
	return st, nil
}

// RunSync is the whole dance: token, fetch everything, apply everything.
func RunSync(db *sql.DB) (Stats, error) {
	var st Stats
	cfg, err := LoadConfig()
	if err != nil {
		return st, err
	}
	token, err := getAccessToken(cfg)
	if err != nil {
		return st, err
	}
	sinceISO := time.Now().UTC().
		AddDate(0, 0, -cfg.Ebay.SyncDays).Format("2006-01-02T15:04:05.000Z")
	listings, err := FetchActiveListings(cfg, token)
	if err != nil {
		return st, err
	}
	orders, err := FetchOrders(cfg, token, sinceISO)
	if err != nil {
		return st, err
	}
	fees, err := FetchFees(cfg, token, sinceISO)
	if err != nil {
		return st, err
	}
	labels, err := FetchLabelCosts(cfg, token, sinceISO)
	if err != nil {
		return st, err
	}
	for _, step := range []func() (Stats, error){
		func() (Stats, error) { return ApplyListings(db, listings) },
		func() (Stats, error) { return ApplyOrders(db, orders) },
		func() (Stats, error) { return ApplyFees(db, fees) },
		func() (Stats, error) { return ApplyLabels(db, labels) },
	} {
		s, err := step()
		if err != nil {
			return st, err
		}
		st.New += s.New
		st.Updated += s.Updated
		st.Sold += s.Sold
		st.Fees += s.Fees
		st.Labels += s.Labels
	}
	return st, nil
}
