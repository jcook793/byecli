// Package ui is the Bubble Tea face of byeCLI: the phosphor table, the
// detail and cost overlays, and the sync plumbing. Rendering lives in view.go.
package ui

import (
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"byecli/core"
	"byecli/ebay"
)

type mode int

const (
	modeTable mode = iota
	modeDetail
	modeCost
	modeSettings
	modeAuth
)

// money columns pair income with its cost: SALE/COST, SHIP/COST
const (
	colItem = iota
	colType
	colEnds
	colSale
	colCost     // cost basis
	colShip     // shipping charged to the buyer
	colShipDiff // charge minus label: +/-/EVEN, the per-item SHIP PROFIT
	colFeeD
	colFeeP
	colNetD
	colNetP
	nCols
)

var titles = [nCols]string{"ITEM", "TYPE", "ENDS", "SALE", "COST",
	"SHIP", "COST", "FEE$", "FEE%", "NET$", "NET%"}

type Model struct {
	db      *sql.DB
	items   []core.Item
	summary core.Summary

	width, height int
	cursor        int
	scroll        int
	sortCol       int
	sortDir       int // 1 asc, -1 desc
	mode          mode
	detail        *core.Item // item shown in the detail overlay
	amber         bool
	notice        string
	noticeErr     bool
	syncing       bool
	costInput     textinput.Model

	cfg        *core.Config // loaded when the settings overlay opens
	testMode   bool         // mirrored from config for the footer badge
	autoDB     bool         // db path wasn't overridden: toggle may swap it
	setCursor  int
	setEditing bool
	setInput   textinput.Model
	authURL    string // consent URL shown while modeAuth
	authBusy   bool   // token exchange in flight

	// layout notes taken while rendering, for mouse hit-testing
	headerY   int
	rowsY     int
	colSpans  [nCols][2]int // x start/end per column
	pageRows  int
	err       error
}

// New builds the model. autoDB says the db path was auto-chosen (no --db or
// $BYECLI_DB), so the test_mode toggle is allowed to hot-swap ledgers.
func New(db *sql.DB, autoDB bool) *Model {
	ti := textinput.New()
	ti.Placeholder = "0.00"
	ti.CharLimit = 10
	ti.Width = 12
	si := textinput.New()
	si.Width = 44 // refresh tokens run long; CharLimit stays unlimited
	m := &Model{db: db, autoDB: autoDB, sortCol: colEnds, sortDir: 1,
		costInput: ti, setInput: si}
	if cfg, err := core.LoadConfig(); err == nil {
		m.testMode = cfg.TestMode
	}
	m.reload()
	return m
}

func (m *Model) reload() {
	items, err := core.FetchItems(m.db)
	if err != nil {
		m.err = err
		return
	}
	var prevID int64 = -1
	if m.cursor < len(m.items) && len(m.items) > 0 {
		prevID = m.items[m.cursor].ID
	}
	m.items = items
	m.summary = core.Summarize(items)
	m.resort()
	m.cursor = 0
	for i := range m.items {
		if m.items[i].ID == prevID {
			m.cursor = i
			break
		}
	}
}

// ── sorting (same semantics as the Python faces: blanks always sink) ─────

func sortStr(it *core.Item, col int) (string, bool) {
	switch col {
	case colItem:
		return strings.ToLower(it.Name), true
	case colType:
		return it.Source, true
	case colEnds:
		if it.Sold() {
			return "z" + *it.DateSold, true
		}
		if it.ListingEnd != nil {
			return *it.ListingEnd, true
		}
		return "9999", true
	}
	return "", false
}

func sortNum(it *core.Item, col int) *float64 {
	switch col {
	case colCost:
		v := it.CostBasis
		return &v
	case colSale:
		if it.Sold() {
			v := it.SalePrice
			return &v
		}
		return it.ListingPrice
	case colShip:
		if it.Sold() {
			v := it.ShippingCharged
			return &v
		}
	case colShipDiff:
		if it.ShippingProfit == nil {
			return nil
		}
		v := -*it.ShippingProfit // column shows label cost over/under charge
		return &v
	case colFeeD:
		if it.Sold() {
			v := it.EbayFees
			return &v
		}
		return it.FeesEst
	case colFeeP:
		if it.Sold() {
			if gross := it.SalePrice + it.ShippingCharged; gross > 0 {
				v := it.EbayFees / gross
				return &v
			}
			return nil
		}
		if it.FeesEst != nil && it.ListingPrice != nil && *it.ListingPrice > 0 {
			v := *it.FeesEst / *it.ListingPrice
			return &v
		}
	case colNetD:
		if it.NetProfit != nil {
			return it.NetProfit
		}
		return it.NetEst
	case colNetP:
		return it.ROI
	}
	return nil
}

func (m *Model) resort() {
	valued := make([]core.Item, 0, len(m.items))
	blanks := make([]core.Item, 0)
	for _, it := range m.items {
		if _, isStr := sortStr(&it, m.sortCol); isStr || sortNum(&it, m.sortCol) != nil {
			valued = append(valued, it)
		} else {
			blanks = append(blanks, it)
		}
	}
	col, dir := m.sortCol, m.sortDir
	sort.SliceStable(valued, func(i, j int) bool {
		a, b := &valued[i], &valued[j]
		if sa, ok := sortStr(a, col); ok {
			sb, _ := sortStr(b, col)
			if dir > 0 {
				return sa < sb
			}
			return sa > sb
		}
		na, nb := sortNum(a, col), sortNum(b, col)
		if dir > 0 {
			return *na < *nb
		}
		return *na > *nb
	})
	m.items = append(valued, blanks...) // blanks sink regardless of direction
}

func (m *Model) setSort(col int) {
	if col == m.sortCol {
		m.sortDir = -m.sortDir
	} else {
		m.sortCol, m.sortDir = col, 1
	}
	var prevID int64 = -1
	if m.cursor < len(m.items) {
		prevID = m.items[m.cursor].ID
	}
	m.resort()
	for i := range m.items {
		if m.items[i].ID == prevID {
			m.cursor = i
		}
	}
}

// ── messages ─────────────────────────────────────────────────────────────

type syncDoneMsg struct {
	stats ebay.Stats
	err   error
}

type clearNoticeMsg struct{}

func doSync(db *sql.DB) tea.Cmd {
	return func() tea.Msg {
		stats, err := ebay.RunSync(db)
		return syncDoneMsg{stats, err}
	}
}

func expireNotice() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return clearNoticeMsg{} })
}

func (m *Model) say(s string, isErr bool) tea.Cmd {
	m.notice, m.noticeErr = s, isErr
	return expireNotice()
}

// ── tea.Model ────────────────────────────────────────────────────────────

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case syncDoneMsg:
		m.syncing = false
		if msg.err != nil {
			return m, m.say(msg.err.Error(), true)
		}
		m.reload()
		notice := fmt.Sprintf("SYNCED · %d NEW · %d PRICE · %d SOLD · %d FEES · %d LABELS",
			msg.stats.New, msg.stats.Updated, msg.stats.Sold, msg.stats.Fees,
			msg.stats.Labels)
		if msg.stats.Note != "" {
			notice += " · " + msg.stats.Note
		}
		return m, m.say(notice, false)
	case clearNoticeMsg:
		m.notice = ""
		return m, nil
	case authDoneMsg:
		m.authBusy = false
		if msg.err != nil {
			return m, m.say(strings.ToUpper(msg.err.Error()), true)
		}
		m.cfg = msg.cfg
		m.mode = modeSettings
		return m, m.say(fmt.Sprintf("REFRESH TOKEN SAVED · GOOD FOR ~%d DAYS", msg.days), false)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.KeyMsg:
		switch m.mode {
		case modeCost:
			return m.updateCostKey(msg)
		case modeDetail:
			return m.updateDetailKey(msg)
		case modeSettings:
			return m.updateSettingsKey(msg)
		case modeAuth:
			return m.updateAuthKey(msg)
		default:
			return m.updateTableKey(msg)
		}
	}
	if m.mode == modeCost {
		var cmd tea.Cmd
		m.costInput, cmd = m.costInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) current() *core.Item {
	if m.cursor < 0 || m.cursor >= len(m.items) {
		return nil
	}
	return &m.items[m.cursor]
}

func (m *Model) updateTableKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.items) - 1
	case "enter":
		if it := m.current(); it != nil {
			m.detail = it
			m.mode = modeDetail
		}
	case "c":
		return m, m.openCost()
	case "p":
		m.amber = !m.amber
		SetTerminalBG(m.amber)
	case "s":
		cfg, err := core.LoadConfig()
		if err != nil {
			return m, m.say(strings.ToUpper(err.Error()), true)
		}
		m.cfg = cfg
		m.setCursor = 0
		m.setEditing = false
		m.mode = modeSettings
	case "e":
		if !m.syncing {
			m.syncing = true
			return m, tea.Batch(m.say("SYNCING…", false), doSync(m.db))
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "0", "-":
		// number-row keys sort columns: 1=ITEM … 0=NET$, -=NET%
		n := 11
		if msg.String() != "-" {
			n, _ = strconv.Atoi(msg.String())
			if n == 0 {
				n = 10
			}
		}
		m.setSort(n - 1)
	}
	m.clampScroll()
	return m, nil
}

func (m *Model) openCost() tea.Cmd {
	it := m.current()
	if m.mode == modeDetail {
		it = m.detail
	}
	if it == nil {
		return nil
	}
	m.mode = modeCost
	m.costInput.SetValue(strconv.FormatFloat(it.CostBasis, 'f', 2, 64))
	m.costInput.CursorEnd()
	return m.costInput.Focus()
}

func (m *Model) updateDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "enter":
		m.mode = modeTable
		m.detail = nil
	case "o":
		if m.detail != nil && m.detail.EbayItemID != nil {
			url := "https://www.ebay.com/itm/" + *m.detail.EbayItemID
			openURL(url)
			return m, m.say("OPENED "+url, false)
		}
		return m, m.say("NO EBAY ITEM ID — SYNC FIRST", true)
	case "c":
		return m, m.openCost()
	}
	return m, nil
}

func (m *Model) updateCostKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeTable
		m.detail = nil
		return m, nil
	case "enter":
		it := m.current()
		if m.detail != nil {
			it = m.detail
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(m.costInput.Value()), 64)
		if err != nil || v < 0 {
			return m, m.say("COST MUST BE A NUMBER ≥ 0", true)
		}
		if err := core.SetCost(m.db, it.ID, v); err != nil {
			return m, m.say(err.Error(), true)
		}
		m.mode = modeTable
		m.detail = nil
		m.reload()
		verdict := "BYE"
		if v > 0 {
			verdict = "FLIP"
		}
		return m, m.say(fmt.Sprintf("COST SET · %s IS A %s", strings.ToUpper(it.Name[:min(20, len(it.Name))]), verdict), false)
	}
	var cmd tea.Cmd
	m.costInput, cmd = m.costInput.Update(msg)
	return m, cmd
}

// ── settings overlay ─────────────────────────────────────────────────────

const (
	easypostKeysURL = "https://www.easypost.com/account/api-keys"
	ebayKeysURL     = "https://developer.ebay.com/my/keys"
	ebayAuthProdURL = "https://developer.ebay.com/my/auth/?env=production&index=0"
	ebayAuthSandURL = "https://developer.ebay.com/my/auth/?env=sandbox&index=0"
	ebaySandUserURL = "https://developer.ebay.com/my/sandbox/user"
)

// settingField maps one config.json key onto the overlay: which section it
// renders under, how to read and write it (with validation), whether the
// list hides its value, and where its value comes from (help shows for the
// selected field; o opens link).
type settingField struct {
	section string // config.json section header
	label   string // the json key, so the overlay and the file speak alike
	empty   string // shown when unset
	secret  bool
	help    string
	link    string
	get     func(c *core.Config) string
	set     func(c *core.Config, v string) error
}

var settingFields = []settingField{
	{section: "general", label: "test_mode", empty: "off · enter toggles",
		help: "eBay sandbox + EasyPost test key + byecli-test.db, all at once.",
		get: func(c *core.Config) string {
			if c.TestMode {
				return "ON"
			}
			return ""
		}}, // enter toggles it in place; set is never called
	{section: "ebay", label: "client_id", empty: "<NOT SET>",
		help: "developer.ebay.com → Application Keys → PRODUCTION column → App ID.",
		link: ebayKeysURL,
		get:  func(c *core.Config) string { return c.Ebay.ClientID },
		set:  func(c *core.Config, v string) error { c.Ebay.ClientID = v; return nil }},
	{section: "ebay", label: "client_secret", empty: "<NOT SET>", secret: true,
		help: "Same row of the Application Keys page → Cert ID.",
		link: ebayKeysURL,
		get:  func(c *core.Config) string { return c.Ebay.ClientSecret },
		set:  func(c *core.Config, v string) error { c.Ebay.ClientSecret = v; return nil }},
	{section: "ebay", label: "ru_name", empty: "<NOT SET>",
		help: "User Tokens (production) → Get a Token via Your App → OAuth → RuName.",
		link: ebayAuthProdURL,
		get:  func(c *core.Config) string { return c.Ebay.RuName },
		set:  func(c *core.Config, v string) error { c.Ebay.RuName = v; return nil }},
	{section: "ebay", label: "refresh_token", secret: true,
		empty: "<NOT SET> · press a to authorize",
		help:  "Minted by the a flow — nothing to look up or paste by hand.",
		get:   func(c *core.Config) string { return c.Ebay.RefreshToken },
		set:   func(c *core.Config, v string) error { c.Ebay.RefreshToken = v; return nil }},
	{section: "ebay", label: "test_client_id", empty: "<NOT SET>",
		help: "Application Keys page again — the SANDBOX column this time → App ID.",
		link: ebayKeysURL,
		get:  func(c *core.Config) string { return c.Ebay.TestClientID },
		set:  func(c *core.Config, v string) error { c.Ebay.TestClientID = v; return nil }},
	{section: "ebay", label: "test_client_secret", empty: "<NOT SET>", secret: true,
		help: "SANDBOX column of the Application Keys page → Cert ID.",
		link: ebayKeysURL,
		get:  func(c *core.Config) string { return c.Ebay.TestClientSecret },
		set:  func(c *core.Config, v string) error { c.Ebay.TestClientSecret = v; return nil }},
	{section: "ebay", label: "test_ru_name", empty: "<NOT SET>",
		help: "User Tokens with env=SANDBOX → OAuth → RuName (separate from prod).",
		link: ebayAuthSandURL,
		get:  func(c *core.Config) string { return c.Ebay.TestRuName },
		set:  func(c *core.Config, v string) error { c.Ebay.TestRuName = v; return nil }},
	{section: "ebay", label: "test_refresh_token", secret: true,
		empty: "<NOT SET> · a while test_mode is on",
		help:  "a flow with test_mode ON — sign in as a SANDBOX TEST USER (o creates one).",
		link:  ebaySandUserURL,
		get:   func(c *core.Config) string { return c.Ebay.TestRefreshToken },
		set:   func(c *core.Config, v string) error { c.Ebay.TestRefreshToken = v; return nil }},
	{section: "ebay", label: "sync_days", empty: "90 (default)",
		help: "How far back sync looks for orders, fees, and labels.",
		get: func(c *core.Config) string {
			if c.Ebay.SyncDays == 0 {
				return ""
			}
			return strconv.Itoa(c.Ebay.SyncDays)
		},
		set: func(c *core.Config, v string) error {
			if v == "" {
				c.Ebay.SyncDays = 0
				return nil
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("sync_days must be a positive number")
			}
			c.Ebay.SyncDays = n
			return nil
		}},
	{section: "easypost", label: "api_key", secret: true,
		empty: "<NOT SET> · " + hyperlink(easypostKeysURL, "easypost.com/account/api-keys"),
		help:  "easypost.com → Account Settings → API Keys → the production EZAK… key.",
		link:  easypostKeysURL,
		get:   func(c *core.Config) string { return c.EasyPost.APIKey },
		set:   func(c *core.Config, v string) error { c.EasyPost.APIKey = v; return nil }},
	{section: "easypost", label: "test_api_key", secret: true,
		empty: "<NOT SET> · the EZTK… one",
		help:  "Same API Keys page → the test EZTK… key.",
		link:  easypostKeysURL,
		get:   func(c *core.Config) string { return c.EasyPost.TestAPIKey },
		set:   func(c *core.Config, v string) error { c.EasyPost.TestAPIKey = v; return nil }},
	{section: "printers", label: "label", empty: "<NOT SET>",
		help: "CUPS queue name for the 4×6 thermal printer (lpstat -p lists them).",
		get:  func(c *core.Config) string { return c.Printers.Label },
		set:  func(c *core.Config, v string) error { c.Printers.Label = v; return nil }},
	{section: "printers", label: "packing_slip", empty: "<NOT SET>",
		help: "CUPS queue name for the full-page laser printer.",
		get:  func(c *core.Config) string { return c.Printers.PackingSlip },
		set:  func(c *core.Config, v string) error { c.Printers.PackingSlip = v; return nil }},
}

func (m *Model) updateSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.setEditing {
		switch msg.String() {
		case "esc":
			m.setEditing = false
			return m, nil
		case "enter":
			f := settingFields[m.setCursor]
			if err := f.set(m.cfg, strings.TrimSpace(m.setInput.Value())); err != nil {
				return m, m.say(strings.ToUpper(err.Error()), true)
			}
			if err := m.cfg.Save(); err != nil {
				return m, m.say(strings.ToUpper(err.Error()), true)
			}
			m.setEditing = false
			return m, m.say("SAVED "+strings.ToUpper(f.label), false)
		}
		var cmd tea.Cmd
		m.setInput, cmd = m.setInput.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "esc", "q":
		m.mode = modeTable
		m.cfg = nil
	case "up", "k":
		if m.setCursor > 0 {
			m.setCursor--
		}
	case "down", "j":
		if m.setCursor < len(settingFields)-1 {
			m.setCursor++
		}
	case "enter":
		f := settingFields[m.setCursor]
		if f.label == "test_mode" { // a switch, not a text field
			if m.syncing {
				return m, m.say("SYNC IN FLIGHT — TOGGLE AFTER IT FINISHES", true)
			}
			m.cfg.TestMode = !m.cfg.TestMode
			if err := m.cfg.Save(); err != nil {
				m.cfg.TestMode = !m.cfg.TestMode
				return m, m.say(strings.ToUpper(err.Error()), true)
			}
			m.testMode = m.cfg.TestMode
			state := "OFF"
			if m.testMode {
				state = "ON"
			}
			if !m.autoDB {
				return m, m.say("TEST MODE "+state+" · KEEPING THE --db/$BYECLI_DB OVERRIDE", false)
			}
			// hot-swap onto the matching ledger so the next sync can't
			// land in the wrong one
			path := core.DBPath()
			if m.testMode {
				path = core.TestDBPath()
			}
			db, err := core.Connect(path)
			if err != nil {
				return m, m.say(strings.ToUpper(err.Error()), true)
			}
			m.db.Close()
			m.db = db
			m.reload()
			return m, m.say("TEST MODE "+state+" · SWITCHED TO "+filepath.Base(path), false)
		}
		m.setEditing = true
		m.setInput.Placeholder = ""
		if f.secret {
			m.setInput.EchoMode = textinput.EchoPassword
			m.setInput.EchoCharacter = '•'
		} else {
			m.setInput.EchoMode = textinput.EchoNormal
		}
		m.setInput.SetValue(f.get(m.cfg))
		m.setInput.CursorEnd()
		return m, m.setInput.Focus()
	case "a":
		m.authURL = "" // instructions first; consent URL built on continue
		m.mode = modeAuth
	case "o":
		if u := settingFields[m.setCursor].link; u != "" {
			openURL(u)
			return m, m.say("OPENED "+u, false)
		}
	}
	return m, nil
}

// ── auth overlay: browser consent → pasted redirect → refresh token ─────

type authDoneMsg struct {
	cfg  *core.Config
	days int
	err  error
}

// doExchange runs off the update loop; it gets its own copy of the config so
// nothing mutates what the view is rendering.
func doExchange(cfg core.Config, pasted string) tea.Cmd {
	return func() tea.Msg {
		days, err := ebay.ExchangeAuthCode(&cfg, pasted)
		return authDoneMsg{&cfg, days, err}
	}
}

// hyperlink wraps text in an OSC 8 terminal hyperlink: clickable (usually
// cmd-click, since the app owns the mouse) where supported, invisible
// zero-width codes everywhere else.
func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// openURL launches the system browser; a var so tests can stub it out.
var openURL = func(u string) {
	cmd := "open"
	if runtime.GOOS != "darwin" {
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, u).Start()
}

func (m *Model) updateAuthKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.authURL == "" { // instructions screen, before any browser opens
		switch msg.String() {
		case "esc", "q":
			m.mode = modeSettings
		case "o":
			openURL("https://developer.ebay.com")
			return m, m.say("OPENED DEVELOPER.EBAY.COM", false)
		case "enter":
			u, err := ebay.ConsentURL(m.cfg)
			if err != nil {
				return m, m.say(strings.ToUpper(err.Error()), true)
			}
			m.authURL = u
			openURL(u)
			m.setInput.EchoMode = textinput.EchoNormal
			m.setInput.Placeholder = "https://…?code=…"
			m.setInput.SetValue("")
			return m, m.setInput.Focus()
		}
		return m, nil
	}
	switch msg.String() { // paste screen
	case "esc":
		if !m.authBusy {
			m.authURL = "" // back to the instructions
		}
		return m, nil
	case "enter":
		if m.authBusy {
			return m, nil
		}
		pasted := strings.TrimSpace(m.setInput.Value())
		if pasted == "" {
			return m, m.say("PASTE THE URL EBAY REDIRECTED YOU TO", true)
		}
		m.authBusy = true
		return m, tea.Batch(m.say("EXCHANGING…", false), doExchange(*m.cfg, pasted))
	}
	var cmd tea.Cmd
	m.setInput, cmd = m.setInput.Update(msg)
	return m, cmd
}

func (m *Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeTable {
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.MouseButtonWheelDown:
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		if msg.Y == m.headerY {
			for col, span := range m.colSpans {
				if msg.X >= span[0] && msg.X < span[1] {
					m.setSort(col)
					break
				}
			}
		} else if row := msg.Y - m.rowsY; row >= 0 && row < m.pageRows {
			if idx := m.scroll + row; idx < len(m.items) {
				if idx == m.cursor { // second click opens detail
					m.detail = &m.items[idx]
					m.mode = modeDetail
				}
				m.cursor = idx
			}
		}
	}
	m.clampScroll()
	return m, nil
}

func (m *Model) clampScroll() {
	if m.pageRows <= 0 {
		return
	}
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.pageRows {
		m.scroll = m.cursor - m.pageRows + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}
