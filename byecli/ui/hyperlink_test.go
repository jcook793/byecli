package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestHyperlinkSurvivesRender(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BYECLI_CONFIG", path)
	os.WriteFile(path, []byte(`{"ebay":{"client_id":"cid"}}`), 0o600)

	m := seededModel(t)
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = mm.(*Model)
	v := m.View()
	if !strings.Contains(v, "\x1b]8;;https://www.easypost.com") {
		t.Error("OSC 8 open sequence missing from View() output")
	}
	if !strings.Contains(v, "easypost.com/account/api-keys") {
		t.Error("link text missing")
	}
}
