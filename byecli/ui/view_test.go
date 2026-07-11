package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"byecli/core"
)

func seededModel(t *testing.T) *Model {
	t.Helper()
	db, err := core.Connect(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`INSERT INTO items (name, source, cost_basis, date_listed, date_sold,
		sale_price, shipping_charged, label_cost, ebay_fees, listing_price, listing_end, ebay_item_id)
		VALUES
		('Sold flip thing', 'flip', 40, '2026-07-01', '2026-07-09', 110, 12, 13.94, 17.85, NULL, NULL, '111'),
		('Active bye thing with a very long name that will need cropping somewhere', 'declutter', 0, '2026-07-03', NULL, 0, 0, 0, 0, 25.0, '2026-07-12T01:02:03.000Z', '222'),
		('Unlisted mystery', 'declutter', 0, NULL, NULL, 0, 0, 0, 0, NULL, NULL, NULL)`)
	m := New(db)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	return mm.(*Model)
}

func TestViewRenders(t *testing.T) {
	m := seededModel(t)
	v := m.View()
	for _, want := range []string{"ITEM", "TYPE", "ENDS ▲", "SHPCHG", "NET$", "NET%",
		"FLIP", "BYE", "$110.00", "—", "NET", "PENDING"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
	if strings.Contains(v, "$0.00\n") {
		t.Error("declutter cost rendered as $0.00, want —")
	}
}

func TestSortAndBlanksSink(t *testing.T) {
	m := seededModel(t)
	m.setSort(colNetD) // toggle to NET$ asc
	last := m.items[len(m.items)-1]
	if last.NetProfit != nil || last.NetEst != nil {
		t.Fatalf("blank did not sink: %+v", last.Name)
	}
	m.setSort(colNetD) // same col flips direction
	if m.sortDir != -1 {
		t.Fatalf("dir: %d", m.sortDir)
	}
	if m.items[0].NetProfit == nil {
		t.Fatalf("desc top: %v", m.items[0].Name)
	}
	// blanks still last even descending
	if name := m.items[len(m.items)-1].Name; name != "Unlisted mystery" {
		t.Fatalf("blank not last desc: %v", name)
	}
}

func TestDetailAndCostOverlays(t *testing.T) {
	m := seededModel(t)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if m.mode != modeDetail {
		t.Fatal("enter did not open detail")
	}
	v := m.View()
	for _, want := range []string{"status", "watchers", "— (sync)", "NET"} {
		if !strings.Contains(v, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	// c chains into the cost editor
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = mm.(*Model)
	if m.mode != modeCost {
		t.Fatal("c did not open cost editor")
	}
	if !strings.Contains(m.View(), "COST BASIS") {
		t.Error("cost panel not rendered")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*Model)
	if m.mode != modeTable {
		t.Fatal("esc did not close")
	}
}

func TestPhosphorToggleAndReload(t *testing.T) {
	m := seededModel(t)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	m = mm.(*Model)
	if !m.amber {
		t.Fatal("p did not toggle phosphor")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = mm.(*Model)
	if m.notice != "RELOADED" {
		t.Fatalf("notice: %q", m.notice)
	}
}
