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

// version is stamped by goreleaser at release time.
var version = "dev"

func main() {
	dbPath := flag.String("db", core.DBPath(),
		"sqlite database path (also via $BYECLI_DB)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("byecli " + version)
		return
	}

	// test mode gets its own ledger so sandbox syncs never touch the real
	// one; an explicit --db or $BYECLI_DB still wins (and pins the db —
	// the settings toggle won't swap away from an explicit choice)
	autoDB := os.Getenv("BYECLI_DB") == "" && *dbPath == core.DBPath()
	if cfg, err := core.LoadConfig(); err == nil && cfg.TestMode && autoDB {
		*dbPath = core.TestDBPath()
	}

	db, err := core.Connect(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "byecli: can't open %s: %v\n", *dbPath, err)
		os.Exit(1)
	}
	defer db.Close()

	ui.SetTerminalBG(false) // green phosphor bg; restored on the way out
	defer ui.ResetTerminalBG()

	p := tea.NewProgram(ui.New(db, autoDB), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		ui.ResetTerminalBG()
		fmt.Fprintf(os.Stderr, "byecli: %v\n", err)
		os.Exit(1)
	}
}
