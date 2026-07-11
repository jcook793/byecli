// Package ui is the Bubble Tea face of byeCLI: the phosphor table, the
// detail and cost overlays, and the sync plumbing. Rendering lives in view.go.
package ui

import (
	"database/sql"
	"fmt"
	"os/exec"
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
)

const (
	colItem = iota
	colType
	colEnds
	colCost
	colSale
	colShpChg
	colShpCst
	colFees
	colNetD
	colNetP
	nCols
)

var titles = [nCols]string{"ITEM", "TYPE", "ENDS", "COST", "SALE",
	"SHPCHG", "SHPCST", "FEES", "NET$", "NET%"}

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

	// layout notes taken while rendering, for mouse hit-testing
	headerY   int
	rowsY     int
	colSpans  [nCols][2]int // x start/end per column
	pageRows  int
	err       error
}

func New(db *sql.DB) *Model {
	ti := textinput.New()
	ti.Placeholder = "0.00"
	ti.CharLimit = 10
	ti.Width = 12
	m := &Model{db: db, sortCol: colEnds, sortDir: 1, costInput: ti}
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
	case colShpChg:
		if it.Sold() {
			v := it.ShippingCharged
			return &v
		}
	case colShpCst:
		if it.Sold() {
			v := it.LabelCost
			return &v
		}
	case colFees:
		if it.Sold() {
			v := it.EbayFees
			return &v
		}
		return it.FeesEst
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
		return m, m.say(fmt.Sprintf("SYNCED · %d NEW · %d PRICE · %d SOLD · %d FEES · %d LABELS",
			msg.stats.New, msg.stats.Updated, msg.stats.Sold, msg.stats.Fees,
			msg.stats.Labels), false)
	case clearNoticeMsg:
		m.notice = ""
		return m, nil
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.KeyMsg:
		switch m.mode {
		case modeCost:
			return m.updateCostKey(msg)
		case modeDetail:
			return m.updateDetailKey(msg)
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
	case "r":
		m.reload()
		return m, m.say("RELOADED", false)
	case "s":
		if !m.syncing {
			m.syncing = true
			return m, tea.Batch(m.say("SYNCING…", false), doSync(m.db))
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "0":
		// number keys sort columns: 1=ITEM … 9=NET$, 0=NET%
		n, _ := strconv.Atoi(msg.String())
		if n == 0 {
			n = 10
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
			_ = exec.Command("open", url).Start()
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
