package ebay

// The auth flow, driven from the settings overlay: ConsentURL builds the
// browser URL, the user signs in and pastes the redirect back, and
// ExchangeAuthCode turns it into a refresh token in the config. Port of the
// retired tools/get_refresh_token.py.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"byecli/core"
)

// consent pages live on auth.ebay.com; the token API stays on hosts[].auth
var consentHosts = map[string]string{
	"production": "https://auth.ebay.com",
	"sandbox":    "https://auth.sandbox.ebay.com",
}

// ConsentURL validates the credentials the flow needs and returns the eBay
// consent page to open. Prereqs: client_id, client_secret, and ru_name in
// the config (the RuName comes from the developer portal under User Tokens →
// Get a Token from eBay via Your Application → OAuth).
func ConsentURL(cfg *core.Config) (string, error) {
	creds := cfg.EbayCreds()
	for name, v := range map[string]string{
		"client_id": creds.ClientID, "client_secret": creds.ClientSecret,
		"ru_name": creds.RuName,
	} {
		if v == "" || strings.HasPrefix(v, "YOUR-") {
			return "", fmt.Errorf("fill in %s first", fieldName(cfg, name))
		}
	}
	return consentHosts[creds.Env] + "/oauth2/authorize?" + url.Values{
		"client_id":     {creds.ClientID},
		"response_type": {"code"},
		"redirect_uri":  {creds.RuName},
		"scope":         {scopes},
	}.Encode(), nil
}

// ExchangeAuthCode takes the URL eBay redirected to (or a bare auth code),
// exchanges it, and saves the refresh token into the config. Returns how
// many days the token lives.
func ExchangeAuthCode(cfg *core.Config, pasted string) (int, error) {
	code := parseAuthCode(strings.TrimSpace(pasted))
	if code == "" {
		return 0, fmt.Errorf("that didn't contain an auth code")
	}
	creds := cfg.EbayCreds()
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {creds.RuName},
	}
	req, _ := http.NewRequest("POST",
		hosts[creds.Env].auth+"/identity/v1/oauth2/token",
		strings.NewReader(form.Encode()))
	req.SetBasicAuth(creds.ClientID, creds.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("network error exchanging the code: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, trim(body))
	}
	var tokens struct {
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil || tokens.RefreshToken == "" {
		return 0, fmt.Errorf("token response made no sense: %s", trim(body))
	}
	cfg.SetRefreshToken(tokens.RefreshToken)
	if err := cfg.Save(); err != nil {
		return 0, err
	}
	return tokens.ExpiresIn / 86400, nil
}

// parseAuthCode digs the ?code= out of the pasted redirect URL; a bare code
// pasted on its own works too.
func parseAuthCode(pasted string) string {
	if u, err := url.Parse(pasted); err == nil {
		if c := u.Query().Get("code"); c != "" {
			return c
		}
	}
	return pasted
}
