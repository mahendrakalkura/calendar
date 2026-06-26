# AGENTS.md

Guidance for AI coding agents working in this repository.

## What this is

A single-binary Go CLI (`main.go`) that fetches calendar events across several Google accounts for a window around today (-3 to +8 days) and prints them as one merged, deduplicated table. Results are cached on disk.

## Build, run, lint

```bash
make build          # go mod tidy + go build -o ./main .
make run            # build then run, IST timezone (default)
make run PST        # run in a different timezone (abbreviation)
make run IST force  # bypass the cache and refetch
make lint           # golangci-lint run ./...
```

There are no automated tests. After any change to `main.go`, run `make build` and `make lint` and confirm both pass before declaring done.

## How it works

- `credentials.json` (Google OAuth client, `installed` type) is required and read at startup (`main.go`).
- `accounts.json` lists accounts; each has `name`, `calendar_id`, and `token_file`.
- Per-account OAuth tokens live under `tokens/`. On first use or an `invalid_grant`, the CLI runs a local-loopback OAuth flow, opens a browser, and writes the refreshed token back to its `token_file`.
- Events are cached in `events.json` keyed by `calendar_id|week_start` with a 60-minute TTL.
- Timezone abbreviations map to IANA zones in `resolveTimezone`.

## Files that must never be committed

These hold real secrets or personal data and are already in `.gitignore`. Never stage, move, or print their contents:

```
accounts.json        credentials.json     events.json
tokens/              gcloud/              main (binary)
```

When adding examples, use `accounts.example.json` (templated, no real data) as the model.

## House rules

- Keep code in the single `main.go`; do not introduce packages or abstractions unless asked.
- Order declarations alphabetically within their kind (types, consts, vars, funcs), matching the existing file.
- Match the existing error-handling style: `log.Fatalf` for startup failures, returned errors inside fetch paths.
- Make surgical changes only - touch what the task requires.
