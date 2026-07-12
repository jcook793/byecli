// Package core is the shared heart of byeCLI: schema, money math, and the
// computed fields both the table and the detail view feed on. It is a port of
// tracker_core.py and reads/writes the exact same SQLite schema, so the Python
// faces and this one can share a database file during the transition.
package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// eBay final value fee, calibrated to real sales (13.6% tier + $0.40 per
// order, verified to the penny on order 19-14861-95485). Estimates still run
// low pre-sale because eBay also takes its cut of shipping and sales tax —
// hence the ~ in the UI. Actual fees replace the estimate at sync time.
const (
	FVFRate  = 0.136
	FVFFixed = 0.40
)

const Schema = `
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
`

// DBPath resolves where the database lives: $BYECLI_DB wins, then
// $XDG_DATA_HOME/byecli/byecli.db, then ~/.local/share/byecli/byecli.db.
func DBPath() string {
	if p := os.Getenv("BYECLI_DB"); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "byecli.db")
}

// TestDBPath is the sandbox ledger, kept apart so test-mode syncs never
// touch the real one. Explicit $BYECLI_DB / --db overrides still win.
func TestDBPath() string {
	if p := os.Getenv("BYECLI_DB"); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "byecli-test.db")
}

func dataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "byecli")
}

// ConfigPath resolves the eBay credentials file: $BYECLI_CONFIG wins, then
// $XDG_CONFIG_HOME/byecli/config.json, then ~/.config/byecli/config.json.
func ConfigPath() string {
	if p := os.Getenv("BYECLI_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "byecli", "config.json")
}

// Connect opens (creating if needed) the database at path, in WAL mode with
// the schema applied.
func Connect(path string) (*sql.DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// one connection: keeps :memory: coherent and matches single-user reality
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(Schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Item is a row of the items table plus every computed field the UI shows.
// Pointer fields are nullable: nil renders as — and sinks to the bottom of
// any sort.
type Item struct {
	ID              int64
	Name            string
	Source          string // "flip" or "declutter" (shown as BYE)
	CostBasis       float64
	DateListed      *string
	DateSold        *string
	SalePrice       float64
	ShippingCharged float64
	LabelCost       float64
	EbayFees        float64
	OtherCosts      float64
	Notes           string
	ListingPrice    *float64
	ListingEnd      *string
	EbayItemID      *string
	EbayOrderID     *string
	Meta            map[string]any

	Status          string // "sold", "listed", "not listed"
	EndsLabel       string // "Tue 7:38p" / "Jul 14", empty when sold/unknown
	SoldLabel       string // "Jun 29"
	FeesEst         *float64
	NetEst          *float64
	ShippingProfit  *float64
	NetProfit       *float64
	ROI             *float64 // return on cost, flips only; est until sold
}

func (it *Item) Sold() bool { return it.DateSold != nil }

func f(v float64) *float64 { return &v }

// Money renders 1234.5 as $1,234.50, negatives as -$…, nil as —.
func Money(v *float64) string {
	if v == nil {
		return "—"
	}
	sign := ""
	if *v < 0 {
		sign = "-"
	}
	whole, frac := math.Modf(math.Abs(*v) + 1e-9)
	intPart := strconv.FormatFloat(whole, 'f', 0, 64)
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s$%s.%02d", sign, b.String(), int(math.Round(frac*100)))
}

func parseISO(iso string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return time.Time{}, false
	}
	return t.Local(), true
}

func hour12(t time.Time) string {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	ap := "a"
	if t.Hour() >= 12 {
		ap = "p"
	}
	return fmt.Sprintf("%d:%02d%s", h, t.Minute(), ap)
}

// EndsLabel gives "Tue 7:38p" for auction ends within a week, "Jul 14" beyond.
func EndsLabel(iso string, now time.Time) string {
	t, ok := parseISO(iso)
	if !ok {
		return ""
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	that := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	if that.Sub(today).Hours() < 7*24 {
		return t.Format("Mon") + " " + hour12(t)
	}
	return t.Format("Jan 2")
}

// FullStamp gives "Sat Jul 11 11:15a" in local time.
func FullStamp(iso string) string {
	t, ok := parseISO(iso)
	if !ok {
		return ""
	}
	return t.Format("Mon Jan 2") + " " + hour12(t)
}

func enrich(it *Item, metaJSON *string, now time.Time) {
	it.Meta = map[string]any{}
	if metaJSON != nil {
		_ = json.Unmarshal([]byte(*metaJSON), &it.Meta)
	}
	switch {
	case it.Sold():
		it.Status = "sold"
	case it.DateListed != nil:
		it.Status = "listed"
	default:
		it.Status = "not listed"
	}
	if !it.Sold() && it.ListingEnd != nil {
		it.EndsLabel = EndsLabel(*it.ListingEnd, now)
	}
	if it.Sold() {
		if d, err := time.Parse("2006-01-02", *it.DateSold); err == nil {
			it.SoldLabel = d.Format("Jan 2")
		}
	}
	if !it.Sold() && it.ListingPrice != nil && *it.ListingPrice != 0 {
		it.FeesEst = f(FVFRate**it.ListingPrice + FVFFixed)
		// shipping intentionally excluded: no real label cost yet, assume a wash
		it.NetEst = f(*it.ListingPrice - it.CostBasis - *it.FeesEst)
	}
	if it.Sold() {
		it.ShippingProfit = f(it.ShippingCharged - it.LabelCost)
		it.NetProfit = f(it.SalePrice + it.ShippingCharged - it.CostBasis -
			it.LabelCost - it.EbayFees - it.OtherCosts)
	}
	net := it.NetProfit
	if !it.Sold() {
		net = it.NetEst
	}
	if it.Source == "flip" && it.CostBasis > 0 && net != nil {
		it.ROI = f(*net / it.CostBasis)
	}
}

// FetchItems loads and enriches every item, sold-last then newest-first,
// matching the Python core's default ordering.
func FetchItems(db *sql.DB) ([]Item, error) {
	rows, err := db.Query(`SELECT id, name, source, cost_basis, date_listed,
		date_sold, sale_price, shipping_charged, label_cost, ebay_fees,
		other_costs, notes, listing_price, listing_end, ebay_item_id,
		ebay_order_id, ebay_meta
		FROM items ORDER BY (date_sold IS NOT NULL), date_sold DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var items []Item
	for rows.Next() {
		var it Item
		var meta *string
		if err := rows.Scan(&it.ID, &it.Name, &it.Source, &it.CostBasis,
			&it.DateListed, &it.DateSold, &it.SalePrice, &it.ShippingCharged,
			&it.LabelCost, &it.EbayFees, &it.OtherCosts, &it.Notes,
			&it.ListingPrice, &it.ListingEnd, &it.EbayItemID, &it.EbayOrderID,
			&meta); err != nil {
			return nil, err
		}
		enrich(&it, meta, now)
		items = append(items, it)
	}
	return items, rows.Err()
}

// Summary backs the stat boxes across the top of the screen.
type Summary struct {
	BestName        string
	BestNet         float64
	HasBest         bool
	MonthNet        float64
	MonthCount      int
	SoldCount       int
	Gross           float64
	Fees            float64
	FeesPct         float64
	ShippingProfit  float64
	NetProfit       float64
	FlipCount       int
	FlipProfit      float64
	FlipROI         float64
	DeclutterProfit float64
	ListedCount     int
	InventoryCost   float64
	PendingValue    float64
}

func Summarize(items []Item) Summary {
	var s Summary
	thisMonth := time.Now().Format("2006-01")
	var flipCost float64
	for i := range items {
		it := &items[i]
		if it.Status != "sold" {
			if it.Status == "listed" {
				s.ListedCount++
			}
			s.InventoryCost += it.CostBasis
			if it.ListingPrice != nil {
				s.PendingValue += *it.ListingPrice
			}
			continue
		}
		net := *it.NetProfit
		s.SoldCount++
		s.Gross += it.SalePrice + it.ShippingCharged
		s.Fees += it.EbayFees
		s.ShippingProfit += *it.ShippingProfit
		s.NetProfit += net
		if !s.HasBest || net > s.BestNet {
			s.HasBest, s.BestName, s.BestNet = true, it.Name, net
		}
		if strings.HasPrefix(*it.DateSold, thisMonth) {
			s.MonthNet += net
			s.MonthCount++
		}
		if it.Source == "flip" {
			s.FlipCount++
			s.FlipProfit += net
			flipCost += it.CostBasis
		} else {
			s.DeclutterProfit += net
		}
	}
	if s.Gross != 0 {
		s.FeesPct = s.Fees / s.Gross
	}
	if flipCost != 0 {
		s.FlipROI = s.FlipProfit / flipCost
	}
	return s
}

// SetCost is the inline cost edit; a real cost basis means it was bought to
// resell, so >0 flips the source and 0 sends it back to BYE.
func SetCost(db *sql.DB, id int64, cost float64) error {
	if cost < 0 {
		return fmt.Errorf("cost can't be negative")
	}
	source := "declutter"
	if cost > 0 {
		source = "flip"
	}
	_, err := db.Exec("UPDATE items SET cost_basis=?, source=? WHERE id=?",
		cost, source, id)
	return err
}
