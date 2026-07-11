package core

import (
	"math"
	"testing"
	"time"
)

func TestMoney(t *testing.T) {
	cases := []struct {
		in   *float64
		want string
	}{
		{nil, "—"},
		{f(0), "$0.00"},
		{f(7.5), "$7.50"},
		{f(1234.5), "$1,234.50"},
		{f(1851.14), "$1,851.14"},
		{f(-252.16), "-$252.16"},
		{f(1000000), "$1,000,000.00"},
	}
	for _, c := range cases {
		if got := Money(c.in); got != c.want {
			t.Errorf("Money(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEndsLabel(t *testing.T) {
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.Local)
	// within a week: weekday + time (exact string depends on local tz for a
	// UTC input, so just check the shape via a local-constructed time)
	soon := time.Date(2026, 7, 14, 19, 38, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if got := EndsLabel(soon, now); got != "Tue 7:38p" {
		t.Errorf("EndsLabel(soon) = %q", got)
	}
	far := time.Date(2026, 7, 30, 12, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if got := EndsLabel(far, now); got != "Jul 30" {
		t.Errorf("EndsLabel(far) = %q", got)
	}
	if got := EndsLabel("garbage", now); got != "" {
		t.Errorf("EndsLabel(garbage) = %q", got)
	}
}

func TestEnrichComputedFields(t *testing.T) {
	sold := "2026-07-09"
	it := Item{Source: "flip", CostBasis: 40, DateSold: &sold, SalePrice: 110,
		ShippingCharged: 12, LabelCost: 13.94, EbayFees: 17.85}
	enrich(&it, nil, time.Now())
	if it.Status != "sold" || it.SoldLabel != "Jul 9" {
		t.Fatalf("status/label: %v %v", it.Status, it.SoldLabel)
	}
	wantNet := 110 + 12 - 40 - 13.94 - 17.85
	if math.Abs(*it.NetProfit-wantNet) > 1e-9 {
		t.Fatalf("net: %v", *it.NetProfit)
	}
	if math.Abs(*it.ShippingProfit-(12-13.94)) > 1e-9 {
		t.Fatalf("ship profit: %v", *it.ShippingProfit)
	}
	if math.Abs(*it.ROI-wantNet/40) > 1e-9 {
		t.Fatalf("roi: %v", *it.ROI)
	}

	// active listing: estimates only, shipping deliberately a wash
	listed := "2026-07-01"
	end := "2026-07-12T01:02:03.000Z"
	active := Item{Source: "declutter", DateListed: &listed, ListingPrice: f(230.50),
		ListingEnd: &end}
	enrich(&active, nil, time.Now())
	if active.Status != "listed" || active.NetProfit != nil || active.ROI != nil {
		t.Fatalf("active: %+v", active)
	}
	wantFees := FVFRate*230.50 + FVFFixed
	if math.Abs(*active.FeesEst-wantFees) > 1e-9 {
		t.Fatalf("fees est: %v", *active.FeesEst)
	}
	if math.Abs(*active.NetEst-(230.50-wantFees)) > 1e-9 {
		t.Fatalf("net est: %v", *active.NetEst)
	}
}

func TestSetCostFlipsSource(t *testing.T) {
	db, err := Connect(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec("INSERT INTO items (name, source) VALUES ('thing', 'declutter')")
	if err := SetCost(db, 1, 25); err != nil {
		t.Fatal(err)
	}
	var src string
	db.QueryRow("SELECT source FROM items WHERE id=1").Scan(&src)
	if src != "flip" {
		t.Fatalf("source after cost: %v", src)
	}
	if err := SetCost(db, 1, 0); err != nil {
		t.Fatal(err)
	}
	db.QueryRow("SELECT source FROM items WHERE id=1").Scan(&src)
	if src != "declutter" {
		t.Fatalf("source after zero: %v", src)
	}
	if err := SetCost(db, 1, -5); err == nil {
		t.Fatal("negative cost accepted")
	}
}
