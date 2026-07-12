package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		('Even steven', 'declutter', 0, '2026-07-02', '2026-07-10', 20, 8, 8, 3.12, NULL, NULL, '555'),
		('Unlisted mystery', 'declutter', 0, NULL, NULL, 0, 0, 0, 0, NULL, NULL, NULL)`)
	m := New(db)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	return mm.(*Model)
}

func TestViewRenders(t *testing.T) {
	m := seededModel(t)
	v := m.View()
	for _, want := range []string{"ITEM", "TYPE", "ENDS ▲", "SALE", "SHIP",
		"FEE$", "FEE%", "NET$", "NET%", "FLIP", "BYE", "$110.00", "—", "NET",
		"PENDING", "-$1.94", "14.6%", "EVEN"} {
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

func TestSettingsOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BYECLI_CONFIG", path)
	os.WriteFile(path, []byte(`{"client_id":"me-byecli-PRD-1234567890",
		"client_secret":"PRD-supersecretvalue","keep_me":"yes"}`), 0o600)

	m := seededModel(t)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = mm.(*Model)
	if m.mode != modeSettings {
		t.Fatal(", did not open settings")
	}
	v := m.View()
	for _, want := range []string{"SETTINGS", "EBAY", "EASYPOST", "PRINTERS",
		"client_id", "sync_days", "90 (default)", "<NOT SET>",
		"me-byecli-PRD-1234567890", "••••"} {
		if !strings.Contains(v, want) {
			t.Errorf("settings missing %q", want)
		}
	}
	if strings.Contains(v, "supersecret") || strings.Contains(v, "PRD-s") {
		t.Error("client_secret not fully hidden")
	}

	// walk down to sync_days and set it
	for i, f := range settingFields {
		if f.label == "sync_days" {
			m.setCursor = i
			break
		}
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if !m.setEditing {
		t.Fatal("enter did not start editing")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("30")})
	m = mm.(*Model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if m.setEditing || !strings.Contains(m.notice, "SAVED") {
		t.Fatalf("save failed: editing=%v notice=%q", m.setEditing, m.notice)
	}

	// the flat legacy file comes back nested, extras intact, legacy keys gone
	raw, _ := os.ReadFile(path)
	var saved map[string]any
	json.Unmarshal(raw, &saved)
	eb, _ := saved["ebay"].(map[string]any)
	if eb == nil || eb["sync_days"] != float64(30) {
		t.Errorf("ebay.sync_days not saved: %v", saved)
	}
	if eb["client_secret"] != "PRD-supersecretvalue" {
		t.Error("client_secret mangled in migration")
	}
	if saved["keep_me"] != "yes" {
		t.Error("unknown config key lost")
	}
	if _, ok := saved["client_id"]; ok {
		t.Error("legacy flat key written back")
	}

	// bad value: error notice, still editing after
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	m.setInput.SetValue("banana")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if !m.noticeErr {
		t.Error("banana accepted as sync_days")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*Model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*Model)
	if m.mode != modeTable {
		t.Fatal("esc did not close settings")
	}
}

func TestAuthFromSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BYECLI_CONFIG", path)
	os.WriteFile(path, []byte(`{"ebay":{"client_id":"cid","client_secret":"sec"}}`), 0o600)

	opened := ""
	orig := openURL
	openURL = func(u string) { opened = u }
	t.Cleanup(func() { openURL = orig })

	m := seededModel(t)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = mm.(*Model)

	// a lands on the instructions screen, ✗ flagging the missing ru_name
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = mm.(*Model)
	if m.mode != modeAuth || m.authURL != "" {
		t.Fatalf("a did not open instructions: mode=%v url=%q", m.mode, m.authURL)
	}
	v := m.View()
	for _, want := range []string{"AUTHORIZE EBAY", "developer.ebay.com",
		"developer program", "✗", "✓"} {
		if !strings.Contains(v, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
	if opened != "" {
		t.Errorf("browser opened before continue: %q", opened)
	}
	// continuing without ru_name complains and stays put
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if m.authURL != "" || !m.noticeErr || !strings.Contains(m.notice, "RU_NAME") {
		t.Fatalf("expected ru_name complaint, got url=%q notice=%q", m.authURL, m.notice)
	}

	m.cfg.Ebay.RuName = "my-ru-name"
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if !strings.Contains(opened, "auth.ebay.com/oauth2/authorize") ||
		!strings.Contains(opened, "client_id=cid") {
		t.Errorf("browser opened with %q", opened)
	}
	v = m.View()
	for _, want := range []string{"AUTHORIZE EBAY", "AGREE", "Paste"} {
		if !strings.Contains(v, want) {
			t.Errorf("auth panel missing %q", want)
		}
	}
	// empty paste is rejected; esc steps back to instructions, then settings
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*Model)
	if !m.noticeErr {
		t.Error("empty paste accepted")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*Model)
	if m.mode != modeAuth || m.authURL != "" {
		t.Fatal("esc did not step back to instructions")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(*Model)
	if m.mode != modeSettings {
		t.Fatal("esc did not return to settings")
	}
}

func TestPhosphorToggle(t *testing.T) {
	m := seededModel(t)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	m = mm.(*Model)
	if !m.amber {
		t.Fatal("p did not toggle phosphor")
	}
}
