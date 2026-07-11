"""One-time helper: turn an eBay OAuth consent into a refresh token.

Prereqs: client_id and client_secret filled in ~/.config/byecli/config.json,
and a redirect URL config (RuName) created on the developer portal under
User Tokens -> Get a Token from eBay via Your Application -> OAuth.

Run it (stdlib only, any python3), follow the printed URL, sign in, approve,
then paste the URL eBay redirected you to (it 404s, that's fine — the code
is in the URL). Writes the refresh token back into the config for byecli.
"""
import base64
import json
import os
import urllib.parse
import urllib.request
from pathlib import Path

CONFIG_PATH = Path(
    os.environ.get("BYECLI_CONFIG")
    or Path.home() / ".config" / "byecli" / "config.json"
)

AUTH_HOSTS = {"production": "https://auth.ebay.com", "sandbox": "https://auth.sandbox.ebay.com"}
API_HOSTS = {"production": "https://api.ebay.com", "sandbox": "https://api.sandbox.ebay.com"}

SCOPES = " ".join([
    "https://api.ebay.com/oauth/api_scope",
    "https://api.ebay.com/oauth/api_scope/sell.fulfillment",
    "https://api.ebay.com/oauth/api_scope/sell.finances",
])

cfg = json.loads(CONFIG_PATH.read_text())
env = cfg.get("environment", "production")
for key in ("client_id", "client_secret"):
    if not cfg.get(key) or cfg[key].startswith("YOUR-"):
        raise SystemExit(f"Fill in '{key}' in {CONFIG_PATH} first.")

ru_name = cfg.get("ru_name") or input("RuName (eBay Redirect URL name, e.g. John_Cook-JohnCook-byecli-abcdef): ").strip()

consent = f"{AUTH_HOSTS[env]}/oauth2/authorize?" + urllib.parse.urlencode({
    "client_id": cfg["client_id"],
    "response_type": "code",
    "redirect_uri": ru_name,
    "scope": SCOPES,
})
print("\n1. Open this in your browser, sign in to eBay, and hit Agree:\n")
print(consent)
print("\n2. You'll land on your (probably broken) redirect page. Copy the FULL URL from the address bar.\n")
pasted = input("Paste it here: ").strip()

query = urllib.parse.parse_qs(urllib.parse.urlparse(pasted).query)
code = (query.get("code") or [None])[0] or pasted  # allow pasting the bare code too

basic = base64.b64encode(f"{cfg['client_id']}:{cfg['client_secret']}".encode()).decode()
req = urllib.request.Request(
    f"{API_HOSTS[env]}/identity/v1/oauth2/token",
    data=urllib.parse.urlencode({
        "grant_type": "authorization_code", "code": code, "redirect_uri": ru_name,
    }).encode(),
    headers={"Authorization": f"Basic {basic}",
             "Content-Type": "application/x-www-form-urlencoded"},
)
try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        tokens = json.loads(resp.read())
except urllib.error.HTTPError as e:
    raise SystemExit(f"Token exchange failed ({e.code}): {e.read().decode()[:400]}")

cfg["refresh_token"] = tokens["refresh_token"]
cfg["ru_name"] = ru_name
CONFIG_PATH.write_text(json.dumps(cfg, indent=2) + "\n")
days = tokens.get("refresh_token_expires_in", 0) // 86400
print(f"\nRefresh token saved to {CONFIG_PATH} (expires in ~{days} days).")
print("You're set — hit Sync in the app.")
