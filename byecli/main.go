// byeCLI — say goodbye to your stuff. Terminal tracker for eBay decluttering
// and flips: shipping arbitrage, real fees, net profit.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"byecli/core"
	"byecli/ui"
)

func main() {
	dbPath := flag.String("db", core.DBPath(),
		"sqlite database path (also via $BYECLI_DB)")
	flag.Parse()

	db, err := core.Connect(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "byecli: can't open %s: %v\n", *dbPath, err)
		os.Exit(1)
	}
	defer db.Close()

	ui.SetTerminalBG(false) // green phosphor bg; restored on the way out
	defer ui.ResetTerminalBG()

	p := tea.NewProgram(ui.New(db), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		ui.ResetTerminalBG()
		fmt.Fprintf(os.Stderr, "byecli: %v\n", err)
		os.Exit(1)
	}
}
