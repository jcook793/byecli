package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is everything ~/.config/byecli/config.json can hold, grouped by
// service. Load keeps any keys it doesn't recognize and Save writes them
// back, so hand-added fields survive a round-trip through the settings
// overlay. Configs from before the sections existed (flat keys at the top
// level) are lifted into their sections on load.
//
// test_mode flips the whole app onto the test rails at once: eBay sandbox
// credentials, the EasyPost test key, and a separate database.
type Config struct {
	TestMode bool           `json:"test_mode,omitempty"`
	Ebay     EbayConfig     `json:"ebay,omitzero"`
	EasyPost EasyPostConfig `json:"easypost,omitzero"`
	Printers PrinterConfig  `json:"printers,omitzero"`

	extra map[string]json.RawMessage // unrecognized keys, preserved on Save
}

// EbayConfig holds both credential sets: eBay's sandbox has its own keyset,
// RuName, and refresh token, so the test_* fields are parallel, not shared.
type EbayConfig struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	RuName       string `json:"ru_name,omitempty"`

	TestClientID     string `json:"test_client_id,omitempty"`
	TestClientSecret string `json:"test_client_secret,omitempty"`
	TestRefreshToken string `json:"test_refresh_token,omitempty"`
	TestRuName       string `json:"test_ru_name,omitempty"`

	SyncDays int `json:"sync_days,omitempty"`
}

type EasyPostConfig struct {
	APIKey     string `json:"api_key,omitempty"`      // EZAK…
	TestAPIKey string `json:"test_api_key,omitempty"` // EZTK…
}

type PrinterConfig struct {
	Label       string `json:"label,omitempty"`        // 4×6 thermal queue
	PackingSlip string `json:"packing_slip,omitempty"` // full-page laser queue
}

// EbayCreds is the active eBay credential set — production, or the sandbox
// set when test_mode is on.
type EbayCreds struct {
	Env          string // "production" or "sandbox"
	ClientID     string
	ClientSecret string
	RefreshToken string
	RuName       string
}

func (c *Config) EbayCreds() EbayCreds {
	if c.TestMode {
		return EbayCreds{"sandbox", c.Ebay.TestClientID, c.Ebay.TestClientSecret,
			c.Ebay.TestRefreshToken, c.Ebay.TestRuName}
	}
	return EbayCreds{"production", c.Ebay.ClientID, c.Ebay.ClientSecret,
		c.Ebay.RefreshToken, c.Ebay.RuName}
}

// SetRefreshToken writes a freshly minted token into whichever slot
// test_mode selects.
func (c *Config) SetRefreshToken(tok string) {
	if c.TestMode {
		c.Ebay.TestRefreshToken = tok
	} else {
		c.Ebay.RefreshToken = tok
	}
}

// EasyPostKey returns the key test_mode selects.
func (c *Config) EasyPostKey() string {
	if c.TestMode {
		return c.EasyPost.TestAPIKey
	}
	return c.EasyPost.APIKey
}

// legacyKeys are the flat pre-section spellings; they migrate into their
// sections on load and never get written back.
var legacyKeys = []string{"environment", "client_id", "client_secret",
	"refresh_token", "ru_name", "sync_days", "easypost_api_key",
	"printer_label", "printer_slip"}

// LoadConfig reads ConfigPath(). A missing file is an empty config, not an
// error — the settings overlay and the auth flow both start from nothing.
func LoadConfig() (*Config, error) {
	path := ConfigPath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", path, err)
	}

	var flat struct {
		Environment  string `json:"environment"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
		RuName       string `json:"ru_name"`
		SyncDays     int    `json:"sync_days"`
		EasyPostKey  string `json:"easypost_api_key"`
		PrinterLabel string `json:"printer_label"`
		PrinterSlip  string `json:"printer_slip"`
		Ebay         struct {
			Environment string `json:"environment"`
		} `json:"ebay"`
	}
	_ = json.Unmarshal(raw, &flat)
	lift := func(dst *string, old string) {
		if *dst == "" {
			*dst = old
		}
	}
	lift(&cfg.Ebay.ClientID, flat.ClientID)
	lift(&cfg.Ebay.ClientSecret, flat.ClientSecret)
	lift(&cfg.Ebay.RefreshToken, flat.RefreshToken)
	lift(&cfg.Ebay.RuName, flat.RuName)
	lift(&cfg.EasyPost.APIKey, flat.EasyPostKey)
	lift(&cfg.Printers.Label, flat.PrinterLabel)
	lift(&cfg.Printers.PackingSlip, flat.PrinterSlip)
	if cfg.Ebay.SyncDays == 0 {
		cfg.Ebay.SyncDays = flat.SyncDays
	}
	// the old environment field became the test_mode switch
	if flat.Environment == "sandbox" || flat.Ebay.Environment == "sandbox" {
		cfg.TestMode = true
	}

	var all map[string]json.RawMessage
	_ = json.Unmarshal(raw, &all)
	known, _ := json.Marshal(&cfg)
	var knownKeys map[string]json.RawMessage
	_ = json.Unmarshal(known, &knownKeys)
	for k := range knownKeys {
		delete(all, k)
	}
	for _, k := range legacyKeys {
		delete(all, k)
	}
	cfg.extra = all
	return &cfg, nil
}

// Save writes the config back to ConfigPath(), owner-readable only.
func (c *Config) Save() error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	for k, v := range c.extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}
