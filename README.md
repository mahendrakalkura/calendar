# calendar

A CLI that prints calendar events across multiple Google accounts in a single merged table. It shows a window around today (3 days back through 8 days ahead), deduplicates events that appear on more than one calendar, and highlights the next upcoming event.

## Setup

### 1. OAuth client

Create an OAuth client of type "Desktop app" in the Google Cloud console with the Calendar API enabled, download its JSON, and save it as `credentials.json` in the repo root:

```json
{
  "installed": {
    "client_id": "...",
    "client_secret": "...",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "redirect_uris": ["http://localhost"]
  }
}
```

### 2. accounts.json

Copy `accounts.example.json` to `accounts.json` and list one entry per calendar:

```json
{
  "accounts": [
    {
      "name": "account-1",
      "token_file": "tokens/token-account-1.json",
      "calendar_id": "primary",
      "authuser": "0"
    },
    {
      "name": "account-2",
      "token_file": "tokens/token-account-2.json",
      "calendar_id": "primary",
      "authuser": "1"
    }
  ]
}
```

- `name` - label for logs and re-auth prompts.
- `token_file` - where this account's OAuth token is stored (created on first run).
- `calendar_id` - `primary`, or a specific calendar address.
- `authuser` - optional. The Google account index used to rewrite `meet.google.com` links so they open under the right session in a multi-account browser. Omit it if you do not need this.

### 3. Authorize

On first run (or whenever a token is rejected) the CLI starts a local-loopback OAuth flow, opens your browser, and writes the resulting token to that account's `token_file` under `tokens/`. These token files contain refresh tokens and are kept out of git via `.gitignore`.

## Caching

Results are cached in `events.json` with a 60-minute TTL, keyed by `calendar_id|week_start`.

- `make run` uses the cache when fresh.
- `make run force` bypasses the cache and refetches.

## Usage

```bash
make run            # IST (default)
make run PST        # different timezone abbreviation
make run IST force  # refetch, bypass cache
```
