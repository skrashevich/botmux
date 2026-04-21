# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build (pure Go, no CGO required)
go build -o botmux .

# Run with token (registers CLI bot)
./botmux -token "BOT_TOKEN"

# Run without token (uses bots from database only)
./botmux

# Env var also works: TELEGRAM_BOT_TOKEN="..." ./botmux
# Flags: -addr :8080, -db botdata.db, -webhook URL
```

No linter configured. Single `go build` produces the binary. Tests live in `tests/` (`go test ./tests/...`).

## Architecture

Multi-package Go app organized into `internal/`, `pkg/`, and root `package main`:

- **main.go** — Entry point. Wires up store, proxy, server, bridge. Token is optional.
- **demo.go** — Demo mode seeding with fake bots and `demo:demo` admin.
- **internal/models/** — Shared data types (`BotConfig`, `Message`, `Chat`, `AuthUser`, `Route`, `BridgeConfig`, `LLMConfig`, etc.)
- **internal/store/** — `Store` struct, SQLite with WAL mode. All DB operations, schema migrations.
- **internal/auth/** — Pure auth functions: `HashPassword`, `CheckPassword`, `GenerateSessionToken`, `GenerateAPIKey`, `HashAPIKey`.
- **internal/bot/** — `Bot` struct wrapping `OvyFlash/telegram-bot-api`. All Telegram API calls. `ProcessUpdate()` dispatches to message/chat/member handlers.
- **internal/llm/** — `Router` for AI-based message routing via OpenAI-compatible API.
- **internal/proxy/** — `Manager` manages ALL bots uniformly. Polling, forwarding, health checks, `UpdateQueue` for long-poll.
- **internal/bridge/** — `Manager` for multi-protocol bridges (generic webhook + Slack). Translates external messages to Telegram format.
- **internal/server/** — HTTP server with `embed.FS` for SPA. REST API, auth middleware, Telegram API proxy at `/tgapi/`.
- **internal/server/templates/index.html** — Complete SPA (vanilla JS). Dark/light theme, i18n (EN/RU). Compiled via `//go:embed`.
- **pkg/logbuf/** — Thread-safe log ring buffer with SSE subscriber support.
- **internal/version/** — GitHub release version checker with caching.

## Key Design Decisions

**No CLI vs web bot distinction**: All bots are functionally identical. The `source` field ('cli' or 'web') is informational only. Both types support management, proxy, and all configuration options. ProxyManager handles polling for all bots. Token is optional at startup.

**Multi-bot with unified table**: Single `bots` table with `manage_enabled` and `proxy_enabled` flags. Each bot can be proxy-only, management-only, or both.

**Chat isolation**: `chats` table uses compound PK `(bot_id, id)`. Messages and tags are keyed by `chat_id` (globally unique in Telegram).

**Bot resolution**: API endpoints accept `bot_id` param. Server checks registered bots map first, then ProxyManager's managed bots (`resolveBot` fallback chain).

**Telegram API proxy** (`/tgapi/bot{TOKEN}/{method}`): Reverse-proxies backend API calls to `api.telegram.org`. For send methods (`sendMessage`, `sendPhoto`, etc.), parses the Telegram response to extract the sent `Message` and saves it to DB. This captures outgoing bot messages that don't appear in `getUpdates`.

**Webhook mode**: Bots marked via `SetWebhookMode()` are not polled by ProxyManager but still show as running via `IsRunning()`. `WebhookHandler()` supports both management processing and proxy forwarding.

**Media handling**: Messages store `media_type` and `file_id`. `bot.go:extractMedia()` detects photo/video/animation/sticker/voice/audio/document/video_note from Telegram updates. `server.go:captureSentMessage` extracts media from API responses. `/api/media?file_id=&bot_id=` proxies file downloads from Telegram. Frontend renders images, video players, audio players inline.

## Dependencies

- `github.com/OvyFlash/telegram-bot-api` — Telegram Bot API (actively maintained fork)
- `modernc.org/sqlite` — SQLite driver (pure Go, no CGO)

## Language

Frontend supports English and Russian via i18n system in `templates/index.html`. Translations are in the `i18n` object (keys `en`/`ru`). `t(key)` returns the current language string. `applyLang()` re-renders all static and dynamic content. Language preference stored in localStorage. Comments may be in English or Russian. README is English.

## Screenshots

Screenshots in `screenshots/` use redacted data (fake usernames/URLs). Regenerate with a puppeteer script if UI changes significantly. Use `evaluateOnNewDocument` to monkey-patch `fetch` for data redaction (DOM replacement doesn't work reliably with innerHTML-rendered content).
