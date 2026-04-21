# Telegram API Test Fixtures

Fixtures in this directory are derived from https://core.telegram.org/bots/api examples.

## Snapshot Update Procedure

Any change to a fixture must:
1. Verify the new content matches the current Telegram Bot API spec at the URL documented in `_spec_snapshot.sha256`.
2. Regenerate the snapshot: `go test -run TestFixturesUpdateSnapshot` (OR manually: compute sha256 of each file and update the `.sha256` file).
3. Add an entry to CHANGELOG/PR description explicitly confirming: "checked against Telegram Bot API docs as of YYYY-MM-DD".

## Files

- `update_*.json` — full Telegram `Update` JSON objects.
- `response_*.json` — raw Bot API responses (successful + error paths).
- `file_*.webp/jpg` — small binary samples for media-download testing.
- `_spec_snapshot.sha256` — manifest of expected file hashes.
