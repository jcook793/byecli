package ebay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"byecli/core"
)

func TestParseAuthCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://localhost/accepted?code=v%5E1.1%23abc&expires_in=299", "v^1.1#abc"},
		{"https://localhost/accepted?isAuthSuccessful=true&code=plain123", "plain123"},
		{"v^1.1#bare-code-pasted-directly", "v^1.1#bare-code-pasted-directly"},
	}
	for _, c := range cases {
		if got := parseAuthCode(c.in); got != c.want {
			t.Errorf("parseAuthCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestConsentURL(t *testing.T) {
	cfg := &core.Config{}
	if _, err := ConsentURL(cfg); err == nil {
		t.Error("empty config passed validation")
	}
	cfg.Ebay = core.EbayConfig{ClientID: "cid", ClientSecret: "sec", RuName: "ru"}
	u, err := ConsentURL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://auth.ebay.com/oauth2/authorize",
		"client_id=cid", "redirect_uri=ru", "sell.finances"} {
		if !strings.Contains(u, want) {
			t.Errorf("consent URL missing %q: %s", want, u)
		}
	}
}

func TestExchangeAuthCode(t *testing.T) {
	t.Setenv("BYECLI_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		u, p, _ := r.BasicAuth()
		if u != "cid" || p != "sec" ||
			r.FormValue("grant_type") != "authorization_code" ||
			r.FormValue("code") != "v^1.1#abc" ||
			r.FormValue("redirect_uri") != "ru" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"refresh_token": "new-tok", "refresh_token_expires_in": 47304000})
	}))
	defer srv.Close()
	orig := hosts["sandbox"]
	hosts["sandbox"] = hostSet{auth: srv.URL}
	defer func() { hosts["sandbox"] = orig }()

	cfg := &core.Config{Ebay: core.EbayConfig{Environment: "sandbox",
		ClientID: "cid", ClientSecret: "sec", RuName: "ru"}}
	days, err := ExchangeAuthCode(cfg, "https://localhost/accepted?code=v%5E1.1%23abc")
	if err != nil {
		t.Fatal(err)
	}
	if days != 547 {
		t.Errorf("days: %d", days)
	}
	saved, _ := core.LoadConfig()
	if saved.Ebay.RefreshToken != "new-tok" {
		t.Errorf("token not saved: %+v", saved.Ebay)
	}
}
