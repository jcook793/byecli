package ui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"byecli/core"
)

// semantic colors stay fixed across phosphors, same as the other faces
var (
	cGreen = lipgloss.Color("#3dff8b")
	cRed   = lipgloss.Color("#ff5c57")
	cBlue  = lipgloss.Color("#6ab6ff")
	cTape  = lipgloss.Color("#d7a04d")
	cFlip  = lipgloss.Color("#c792ea")
)

type palette struct {
	bright, ink, muted, dim, cursorBG lipgloss.Color
	bg                                string // terminal default bg via OSC 11
}

var (
	phosphorGreen = palette{"#3dff8b", "#a4ecc2", "#63b184", "#2e5941", "#153420", "#070b08"}
	phosphorAmber = palette{"#ffc24d", "#eec687", "#bb9448", "#5e4a24", "#332507", "#0d0903"}
)

// SetTerminalBG paints the terminal's default background (OSC 11) so the
// whole screen sits on the phosphor-dark bg, like the Python faces. Terminals
// that don't support it simply ignore the sequence.
func SetTerminalBG(amber bool) {
	p := phosphorGreen
	if amber {
		p = phosphorAmber
	}
	fmt.Fprintf(os.Stdout, "\x1b]11;%s\x07", p.bg)
}

// ResetTerminalBG restores the user's own background on exit (OSC 111).
func ResetTerminalBG() {
	os.Stdout.WriteString("\x1b]111\x07")
}

func (m *Model) pal() palette {
	if m.amber {
		return phosphorAmber
	}
	return phosphorGreen
}

func fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// ── cells ────────────────────────────────────────────────────────────────

type cell struct {
	text  string
	color lipgloss.Color
	right bool
}

func (m *Model) cells(it *core.Item) [nCols]cell {
	p := m.pal()
	var c [nCols]cell
	c[colItem] = cell{it.Name, p.ink, false}
	if it.Source == "flip" {
		c[colType] = cell{"FLIP", cFlip, false}
	} else {
		c[colType] = cell{"BYE", cTape, false}
	}
	switch {
	case it.Sold():
		c[colEnds] = cell{"[" + strings.ToUpper(it.SoldLabel) + "]", cGreen, false}
	case it.EndsLabel != "":
		c[colEnds] = cell{"[" + strings.ToUpper(it.EndsLabel) + "]", cBlue, false}
	default:
		c[colEnds] = cell{"[" + strings.ToUpper(it.Status) + "]", p.muted, false}
	}
	if it.Source == "declutter" {
		c[colCost] = cell{"—", p.muted, true}
	} else {
		c[colCost] = cell{core.Money(&it.CostBasis), p.ink, true}
	}
	switch {
	case it.Sold():
		c[colSale] = cell{core.Money(&it.SalePrice), p.ink, true}
	case it.ListingPrice != nil:
		c[colSale] = cell{"~" + core.Money(it.ListingPrice), p.muted, true}
	default:
		c[colSale] = cell{"—", p.muted, true}
	}
	if it.Sold() {
		c[colShpChg] = cell{core.Money(&it.ShippingCharged), p.ink, true}
		c[colShpCst] = cell{core.Money(&it.LabelCost), p.ink, true}
		c[colFees] = cell{core.Money(&it.EbayFees), p.ink, true}
	} else {
		c[colShpChg] = cell{"—", p.muted, true}
		c[colShpCst] = cell{"—", p.muted, true}
		if it.FeesEst != nil {
			c[colFees] = cell{"~" + core.Money(it.FeesEst), p.muted, true}
		} else {
			c[colFees] = cell{"—", p.muted, true}
		}
	}
	switch {
	case it.NetProfit != nil:
		c[colNetD] = cell{core.Money(it.NetProfit), moneyColor(*it.NetProfit), true}
	case it.NetEst != nil:
		col := p.muted
		if *it.NetEst < 0 {
			col = cRed
		}
		c[colNetD] = cell{"~" + core.Money(it.NetEst), col, true}
	default:
		c[colNetD] = cell{"—", p.muted, true}
	}
	switch {
	case it.ROI == nil: // BYE items: no cost basis, no meaningful return
		c[colNetP] = cell{"—", p.muted, true}
	case it.Sold():
		c[colNetP] = cell{pct(*it.ROI), moneyColor(*it.ROI), true}
	default:
		col := p.muted
		if *it.ROI < 0 {
			col = cRed
		}
		c[colNetP] = cell{"~" + pct(*it.ROI), col, true}
	}
	return c
}

func moneyColor(v float64) lipgloss.Color {
	if v < 0 {
		return cRed
	}
	return cGreen
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

// ── view ─────────────────────────────────────────────────────────────────

const gap = 2 // spaces between columns

func (m *Model) View() string {
	if m.err != nil {
		return "byecli: " + m.err.Error() + "\n"
	}
	if m.width == 0 {
		return "warming up the phosphor…"
	}

	stats := m.statsBar()
	statsH := lipgloss.Height(stats)
	m.headerY = statsH + 1 // header sits just under the panel's top border
	m.rowsY = m.headerY + 1
	m.pageRows = m.height - statsH - 4 // panel borders + header + footer
	if m.pageRows < 1 {
		m.pageRows = 1
	}
	m.clampScroll()

	full := strings.Join([]string{stats, m.tablePanel(), m.footerView()}, "\n")
	switch m.mode {
	case modeDetail:
		if m.detail != nil {
			full = m.overlayCentered(full, m.detailPanel())
		}
	case modeCost:
		full = m.overlayCentered(full, m.costPanel())
	}
	return full
}

// column widths: every numeric column gets what its content needs, ITEM
// stretches into whatever is left (the byeCLI take on the stretchy column)
func (m *Model) colWidths(rows [][nCols]cell, contentW int) [nCols]int {
	var w [nCols]int
	for col := 1; col < nCols; col++ {
		w[col] = lipgloss.Width(m.headerTitle(col))
		for _, r := range rows {
			if n := lipgloss.Width(r[col].text); n > w[col] {
				w[col] = n
			}
		}
	}
	used := 0
	for col := 1; col < nCols; col++ {
		used += w[col] + gap
	}
	w[colItem] = contentW - used
	if w[colItem] < 20 {
		w[colItem] = 20
	}
	return w
}

func (m *Model) headerTitle(col int) string {
	t := titles[col]
	if col == m.sortCol {
		if m.sortDir > 0 {
			t += " ▲"
		} else {
			t += " ▼"
		}
	}
	return t
}

func padCell(text string, width int, right bool) string {
	n := lipgloss.Width(text)
	if n > width {
		// crop with ellipsis (item names on narrow terminals)
		r := []rune(text)
		if width > 0 {
			text = string(r[:min(len(r), width-1)]) + "…"
		}
		n = lipgloss.Width(text)
	}
	pad := strings.Repeat(" ", max(0, width-n))
	if right {
		return pad + text
	}
	return text + pad
}

func centerPad(text string, width int) string {
	n := lipgloss.Width(text)
	if n > width {
		text = ansi.Truncate(text, width, "…")
		n = lipgloss.Width(text)
	}
	left := (width - n) / 2
	return strings.Repeat(" ", left) + text + strings.Repeat(" ", width-n-left)
}

// titledPanel draws a rounded border with the title inset in the top edge,
// the byeCLI cousin of Textual's border_title.
func (m *Model) titledPanel(title string, lines []string, w int) string {
	p := m.pal()
	b := fg(p.muted)
	t := fg(p.bright).Bold(true)
	title = cropTitle(title, w)
	inner := w - 2
	fill := inner - lipgloss.Width(title) - 3
	if fill < 0 {
		fill = 0
	}
	out := []string{b.Render("╭─ ") + t.Render(title) + b.Render(" "+strings.Repeat("─", fill)+"╮")}
	for _, ln := range lines {
		n := lipgloss.Width(ln)
		if n > inner-2 { // cramped terminal: crop rather than break the border
			ln = ansi.Truncate(ln, inner-2, "")
			n = inner - 2
		}
		if pad := inner - 2 - n; pad > 0 {
			ln += strings.Repeat(" ", pad)
		}
		out = append(out, b.Render("│ ")+ln+b.Render(" │"))
	}
	out = append(out, b.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

func (m *Model) tablePanel() string {
	p := m.pal()
	rows := make([][nCols]cell, len(m.items))
	for i := range m.items {
		rows[i] = m.cells(&m.items[i])
	}
	contentW := m.width - 4 // panel borders + one space padding each side
	w := m.colWidths(rows, contentW)

	// header (+ remember column x-spans for click-to-sort; +2 for border+pad)
	hdrStyle := fg(p.muted).Bold(true)
	var hdr []string
	x := 2
	for col := 0; col < nCols; col++ {
		m.colSpans[col] = [2]int{x, x + w[col] + gap}
		x += w[col] + gap
		hdr = append(hdr, hdrStyle.Render(padCell(m.headerTitle(col), w[col], col >= colCost)))
	}
	lines := []string{strings.Join(hdr, strings.Repeat(" ", gap))}

	end := min(m.scroll+m.pageRows, len(m.items))
	for i := m.scroll; i < end; i++ {
		var parts []string
		selected := i == m.cursor && m.mode == modeTable
		for col := 0; col < nCols; col++ {
			c := rows[i][col]
			st := fg(c.color)
			if selected {
				st = st.Background(p.cursorBG)
			}
			parts = append(parts, st.Render(padCell(c.text, w[col], c.right)))
		}
		sep := strings.Repeat(" ", gap)
		if selected {
			sep = lipgloss.NewStyle().Background(p.cursorBG).Render(sep)
		}
		lines = append(lines, strings.Join(parts, sep))
	}
	for len(lines) < m.pageRows+1 {
		lines = append(lines, "")
	}
	return m.titledPanel("ITEMS", lines, m.width)
}

// ── stat boxes ───────────────────────────────────────────────────────────

// cropTitle keeps an inset border title from bursting its box: the top edge
// needs 5 cells of chrome (corners, dashes, spaces) around the text.
func cropTitle(title string, boxW int) string {
	maxT := boxW - 5
	if maxT < 1 {
		return ""
	}
	if lipgloss.Width(title) > maxT {
		return ansi.Truncate(title, maxT, "…")
	}
	return title
}

func (m *Model) box(title, content string, w int) string {
	p := m.pal()
	b := fg(p.muted)
	t := fg(p.bright).Bold(true)
	title = cropTitle(title, w)
	inner := w - 2
	fill := inner - lipgloss.Width(title) - 3
	if fill < 0 {
		fill = 0
	}
	top := b.Render("╭─ ") + t.Render(title) + b.Render(" "+strings.Repeat("─", fill)+"╮")
	mid := b.Render("│") + centerPad(content, inner) + b.Render("│")
	bot := b.Render("╰" + strings.Repeat("─", inner) + "╯")
	return top + "\n" + mid + "\n" + bot
}

func (m *Model) statsBar() string {
	s := m.summary
	money := func(v float64) string {
		return fg(moneyColor(v)).Render(core.Money(&v))
	}
	boxes := []struct{ title, content string }{
		{"NET PROFIT", money(s.NetProfit)},
		{"FLIPS", money(s.FlipProfit) + " · " + pct(s.FlipROI)},
		{"BYE", money(s.DeclutterProfit)},
		{"SHIP PROFIT", money(s.ShippingProfit)},
		{"EBAY FEES", fmt.Sprintf("%s · %.1f%%", core.Money(&s.Fees), s.FeesPct*100)},
		{"PENDING", fmt.Sprintf("%s · %d UP", core.Money(&s.PendingValue), s.ListedCount)},
		{"MONTH", fmt.Sprintf("%s · %d", money(s.MonthNet), s.MonthCount)},
	}
	n := len(boxes)
	base := m.width / n
	rem := m.width % n
	var parts []string
	for i, bx := range boxes {
		w := base
		if i < rem { // spread the remainder instead of dumping it on NET
			w++
		}
		parts = append(parts, m.box(bx.title, bx.content, w))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// ── overlay compositing: dim the world, float the panel ─────────────────

// overlayCentered strips the base screen to plain text, repaints it in the
// dim phosphor, and splices the (fully styled) panel into the middle.
func (m *Model) overlayCentered(base, panel string) string {
	p := m.pal()
	dim := fg(p.dim)
	baseLines := strings.Split(base, "\n")
	panelLines := strings.Split(panel, "\n")
	pw := 0
	for _, pl := range panelLines {
		if n := lipgloss.Width(pl); n > pw {
			pw = n
		}
	}
	x := max(0, (m.width-pw)/2)
	y := max(0, (len(baseLines)-len(panelLines))/2)

	for i, line := range baseLines {
		plain := []rune(ansi.Strip(line))
		for len(plain) < m.width {
			plain = append(plain, ' ')
		}
		j := i - y
		if j < 0 || j >= len(panelLines) {
			baseLines[i] = dim.Render(string(plain))
			continue
		}
		pl := panelLines[j]
		if pad := pw - lipgloss.Width(pl); pad > 0 {
			pl += strings.Repeat(" ", pad)
		}
		left := string(plain[:min(len(plain), x)])
		right := ""
		if x+pw < len(plain) {
			right = string(plain[x+pw:])
		}
		baseLines[i] = dim.Render(left) + pl + dim.Render(right)
	}
	return strings.Join(baseLines, "\n")
}

// ── overlays ─────────────────────────────────────────────────────────────

// keyLine renders "key description" pairs — blue keys, muted words — used by
// the footer and the overlay hint lines so they all speak the same dialect.
// When the key is the first letter of its description ("s sync"), the letter
// is highlighted in place ("sync" with a blue s) instead of shown separately.
func (m *Model) keyLine(pairs [][2]string) string {
	mut := fg(m.pal().muted)
	key := fg(cBlue)
	var parts []string
	for _, kv := range pairs {
		k, desc := kv[0], kv[1]
		if len([]rune(k)) == 1 && strings.HasPrefix(desc, k) {
			parts = append(parts, key.Render(k)+mut.Render(desc[len(k):]))
		} else {
			parts = append(parts, key.Render(k)+" "+mut.Render(desc))
		}
	}
	return strings.Join(parts, mut.Render("  · "))
}

func (m *Model) panelStyle() lipgloss.Style {
	p := m.pal()
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(p.bright).
		Padding(1, 4)
}

func (m *Model) detailPanel() string {
	p := m.pal()
	it := m.detail
	mut := fg(p.muted)
	ink := fg(p.ink)

	line := func(label, value string, st lipgloss.Style) string {
		return mut.Render(padCell(label, 13, false)) + st.Render(value)
	}
	metaStr := func(key string) (string, bool) {
		v, ok := it.Meta[key]
		if !ok {
			return "— (sync)", false
		}
		return fmt.Sprintf("%v", v), true
	}

	var left []string
	switch {
	case it.Sold():
		left = append(left, line("status", "SOLD "+it.SoldLabel, fg(cGreen)))
	case it.ListingEnd != nil:
		left = append(left, line("status", "ends "+core.FullStamp(*it.ListingEnd), fg(cBlue)))
	default:
		left = append(left, line("status", strings.ToUpper(it.Status), mut))
	}
	listed := "—"
	if it.DateListed != nil {
		listed = *it.DateListed
	}
	left = append(left, line("listed", listed, ink))
	if !it.Sold() {
		left = append(left, line("asking", core.Money(it.ListingPrice), ink))
	}
	watch, _ := metaStr("watch_count")
	bids, _ := metaStr("bid_count")
	left = append(left, line("watchers", watch, ink), line("bids", bids, ink))
	if it.Sold() {
		buyer, _ := metaStr("buyer")
		shipTo, _ := metaStr("ship_to")
		left = append(left, line("buyer", buyer, ink), line("ship to", shipTo, ink))
	}
	itemID := "—"
	if it.EbayItemID != nil {
		itemID = *it.EbayItemID
	}
	left = append(left, line("ebay #", itemID, ink))
	if it.EbayOrderID != nil {
		left = append(left, line("order", *it.EbayOrderID, ink))
	}

	moneyLine := func(label string, v *float64, est bool, st lipgloss.Style) string {
		s := core.Money(v)
		if est && v != nil {
			s = "~" + s
		}
		return mut.Render(padCell(label, 13, false)) + st.Render(padCell(s, 12, true))
	}
	est := !it.Sold()
	var right []string
	right = append(right, moneyLine("cost", &it.CostBasis, false, ink))
	if it.Sold() {
		right = append(right,
			moneyLine("sale", &it.SalePrice, false, ink),
			moneyLine("ship charge", &it.ShippingCharged, false, ink),
			moneyLine("ship cost", &it.LabelCost, false, ink),
			moneyLine("ebay fees", &it.EbayFees, false, ink),
			moneyLine("other", &it.OtherCosts, false, ink))
	} else {
		right = append(right,
			moneyLine("asking", it.ListingPrice, true, mut),
			moneyLine("ebay fees", it.FeesEst, true, mut))
	}
	right = append(right, mut.Render(strings.Repeat("─", 25)))
	net := it.NetProfit
	if net == nil {
		net = it.NetEst
	}
	netSt := mut
	if net != nil {
		netSt = fg(moneyColor(*net))
	}
	netLine := moneyLine("NET", net, est, netSt)
	if it.ROI != nil {
		netLine += netSt.Render(" " + pct(*it.ROI))
	}
	right = append(right, netLine)

	for len(left) < len(right) {
		left = append(left, "")
	}
	for len(right) < len(left) {
		right = append(right, "")
	}
	cols := lipgloss.JoinHorizontal(lipgloss.Top,
		strings.Join(left, "\n"), strings.Repeat(" ", 6), strings.Join(right, "\n"))

	name := it.Name
	if lipgloss.Width(name) > 64 {
		name = string([]rune(name)[:63]) + "…"
	}
	title := fg(p.bright).Bold(true).Render(name)
	hint := m.keyLine([][2]string{
		{"esc", "close"}, {"o", "open on ebay"}, {"c", "cost"},
	})
	return m.panelStyle().Render(title + "\n\n" + cols + "\n\n" + hint)
}

func (m *Model) costPanel() string {
	p := m.pal()
	title := fg(p.bright).Bold(true).Render("COST BASIS")
	sub := fg(p.muted).Render(">0 marks it a FLIP · 0 sends it back to BYE")
	hint := m.keyLine([][2]string{{"enter", "save"}, {"esc", "cancel"}})
	return m.panelStyle().Render(strings.Join([]string{
		title, sub, "", m.costInput.View(), "", hint}, "\n"))
}

// logo is the wordmark in the bottom-right corner: quiet "bye", bright "CLI",
// and the phosphor block cursor from the web brand.
func (m *Model) logo() string {
	p := m.pal()
	return fg(p.muted).Render("bye") +
		fg(p.bright).Bold(true).Render("CLI") +
		fg(p.bright).Render(" ▊")
}

func (m *Model) footerView() string {
	var line string
	if m.notice != "" {
		st := fg(cGreen)
		if m.noticeErr {
			st = fg(cRed)
		}
		line = st.Render(" " + m.notice)
	} else {
		// keys get the subtle blue, descriptions stay muted — the Footer look
		line = " " + m.keyLine([][2]string{
			{"enter", "detail"}, {"s", "sync"}, {"c", "cost"}, {"p", "phosphor"},
			{"r", "reload"}, {"1-0", "sort"}, {"q", "quit"},
		})
		if m.syncing {
			line = " " + fg(cGreen).Render("SYNCING…") + line
		}
	}
	logo := m.logo()
	pad := m.width - lipgloss.Width(line) - lipgloss.Width(logo) - 1
	if pad >= 1 {
		return line + strings.Repeat(" ", pad) + logo + " "
	}
	if extra := m.width - lipgloss.Width(line); extra > 0 {
		line += strings.Repeat(" ", extra)
	}
	return line
}
