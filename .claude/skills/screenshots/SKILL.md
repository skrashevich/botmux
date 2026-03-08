---
name: screenshots
description: Regenerate project screenshots using Puppeteer script
disable-model-invocation: true
---

# Screenshots

Regenerate all UI screenshots in `screenshots/` directory using the Puppeteer script.

## Steps

1. Ensure the dev server is running on port 8081. If not, start it with `go run . -addr :8081`.
2. Run `node take-screenshots.mjs` to capture all screenshots.
3. Report which screenshots were generated and their file sizes.

## Notes

- The script uses `evaluateOnNewDocument` to monkey-patch `fetch` for data redaction.
- Screenshots use fake usernames/URLs for privacy.
- Requires Puppeteer with Chrome installed (`npx puppeteer browsers install chrome` if missing).
