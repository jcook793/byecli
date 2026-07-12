package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BYECLI_CONFIG", path)

	// missing file: empty config, no error
	cfg, err := LoadConfig()
	if err != nil || cfg.Ebay.ClientID != "" {
		t.Fatalf("fresh load: %v %+v", err, cfg)
	}

	os.WriteFile(path, []byte(`{
		"ebay": {"client_id": "abc", "sync_days": 30},
		"printers": {"label": "zebra"},
		"hand_added_mystery": "keep me"
	}`), 0o600)

	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ebay.ClientID != "abc" || cfg.Ebay.SyncDays != 30 ||
		cfg.Printers.Label != "zebra" {
		t.Fatalf("loaded: %+v", cfg)
	}

	cfg.Ebay.RefreshToken = "tok-123"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perms: %v", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	eb, _ := m["ebay"].(map[string]any)
	if eb == nil || eb["client_id"] != "abc" || eb["refresh_token"] != "tok-123" {
		t.Fatalf("saved: %v", m)
	}
	if m["hand_added_mystery"] != "keep me" {
		t.Error("unknown key lost on save")
	}

	// clearing a field removes it; a fully empty section vanishes
	cfg.Printers.Label = ""
	cfg.Save()
	raw, _ = os.ReadFile(path)
	m = nil
	json.Unmarshal(raw, &m)
	if _, ok := m["printers"]; ok {
		t.Error("empty printers section still in file")
	}

	// invalid JSON is a real error, not an empty config
	os.WriteFile(path, []byte("{nope"), 0o600)
	if _, err := LoadConfig(); err == nil {
		t.Error("invalid JSON loaded without error")
	}
}

func TestConfigMigratesFlatKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("BYECLI_CONFIG", path)

	// a config from before the sections existed
	os.WriteFile(path, []byte(`{
		"environment": "production",
		"client_id": "abc",
		"client_secret": "shh",
		"refresh_token": "tok",
		"ru_name": "ru",
		"sync_days": 45,
		"easypost_api_key": "epkey",
		"printer_label": "zebra",
		"printer_slip": "laser"
	}`), 0o600)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ebay.ClientID != "abc" || cfg.Ebay.ClientSecret != "shh" ||
		cfg.Ebay.RefreshToken != "tok" || cfg.Ebay.RuName != "ru" ||
		cfg.Ebay.SyncDays != 45 || cfg.EasyPost.APIKey != "epkey" ||
		cfg.Printers.Label != "zebra" || cfg.Printers.PackingSlip != "laser" {
		t.Fatalf("migration missed fields: %+v", cfg)
	}

	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var m map[string]any
	json.Unmarshal(raw, &m)
	for _, k := range legacyKeys {
		if _, ok := m[k]; ok {
			t.Errorf("legacy key %q written back", k)
		}
	}
	eb, _ := m["ebay"].(map[string]any)
	if eb == nil || eb["refresh_token"] != "tok" {
		t.Fatalf("nested save wrong: %v", m)
	}

	// nested values win over stale flat leftovers
	os.WriteFile(path, []byte(`{
		"client_id": "old-flat",
		"ebay": {"client_id": "new-nested"}
	}`), 0o600)
	cfg, _ = LoadConfig()
	if cfg.Ebay.ClientID != "new-nested" {
		t.Errorf("flat key beat nested: %q", cfg.Ebay.ClientID)
	}
}
