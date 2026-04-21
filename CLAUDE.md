# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build (pure Go, no CGO required)
go build -o botmux .

# Run with token (registers CLI bot)
./botmux -token "BOT_TOKEN"

# Run without token (uses bots from database only)
./botmux

# Env var also works: TELEGRAM_BOT_TOKEN="..." ./botmux
# Flags: -addr :8080, -db botdata.db, -webhook URL, -tg-api URL, -demo, -version
```

No linter configured. Single `go build` produces the binary.

```bash
# Docker
TELEGRAM_BOT_TOKEN="..." docker compose up -d

# Multi-arch build
docker buildx build --platform linux/amd64,linux/arm64 -t botmux .
```

```bash
# Run tests
go test -v ./...
```

## Architecture

Multi-package Go app organized into `internal/`, `pkg/`, and root `package main`:

### Root (package main)
- **main.go** — Entry point. Wires up all components: store, proxy, server, bridge. Token is optional. Supports `-demo` flag and `-tg-api` for custom Telegram API URL.
- **demo.go** — `seedDemoData()` for demo mode. Populates `demo.db` with `demo:demo` admin and 3 demo bots.

### internal/models — Shared data types
- **models.go** — All shared types: `BotConfig`, `Chat`, `Message`, `AuthUser`, `Route`, `RouteMapping`, `BridgeConfig`, `LLMConfig`, `AdminInfo`, `UserTag`, etc.

### internal/store — Database layer
- **store.go** — `Store` struct, SQLite with WAL mode. All DB operations. Auto-migrates schema on startup. Auth tables, bridge tables, LLM config. Auto-creates default admin on first run.

### internal/auth — Authentication utilities
- **auth.go** — Pure functions: `HashPassword`, `CheckPassword`, `GenerateSessionToken`, `GenerateAPIKey`, `HashAPIKey`. Constants: `SessionCookieName`, `SessionDuration`.

### internal/bot — Telegram Bot wrapper
- **bot.go** — `Bot` struct wrapping `OvyFlash/telegram-bot-api`. All Telegram API calls (send, ban, pin, admin management). `ProcessUpdate()` dispatches to message/chat/member/business/callback handlers (Bot API 9.6 compatible). `OnMessageSent` callback notifies bridges. `AllowedUpdateTypes` defines requested update types.

### internal/llm — LLM-based message routing
- **llm.go** — `Router` for AI-based message routing via OpenAI-compatible API. `RouteMessage()` builds context from all bots+descriptions+chats, calls LLM Chat Completions, parses JSON routing decision.

### internal/proxy — Bot polling & forwarding manager
- **proxy.go** — `Manager` manages ALL bots uniformly. Runs independent `pollLoop` per bot with raw JSON `getUpdates`. Dual-mode per bot: forwards updates to backend URL (proxy) and/or processes them for chat tracking (management). `WebhookHandler()` for bots in webhook mode. `UpdateQueue` ring buffer for long-poll consumers.

### internal/bridge — Multi-protocol bridges
- **bridge.go** — `Manager` for multi-protocol bridges. Translates external messages into Telegram Update JSON format and injects them into `ProcessUpdate()`. Intercepts outgoing bot messages via `OnMessageSent` hook. Maintains chat/message mappings for threading.
- **slack.go** — Native Slack protocol bridge. Handles Slack Events API (URL verification, HMAC-SHA256 signature validation, event parsing). Slack-specific types (`SlackConfig`, `SlackEventPayload`, `SlackMessageEvent`).

### internal/server — HTTP server
- **server.go** — HTTP server with `embed.FS` for SPA. REST API for all bot/chat/message/admin/bridge operations. Auth middleware (cookie sessions + Bearer API keys). Telegram API proxy at `/tgapi/`. Long polling at `/api/updates/poll`.
- **internal/server/templates/index.html** — Complete SPA (vanilla JS). Dark/light theme, i18n (EN/RU). Compiled into binary via `//go:embed`.

### pkg/logbuf — Log buffer utility
- **logbuf.go** — Thread-safe ring buffer (`LogBuffer`) that captures log output for web UI SSE streaming.

### internal/version — Version checking
- **version.go** — `Checker` fetches latest release from GitHub API with 6-hour cache. `CompareSemver()` for version comparison.

### Tests (root, package main)
- **e2e_*_test.go** — End-to-end integration tests with fake Telegram/Slack/LLM servers.
- **server_capture_test.go, longpoll_test.go, version_test.go** — Unit/integration tests.

## Key Design Decisions

**No CLI vs web bot distinction**: All bots are functionally identical. The `source` field ('cli' or 'web') is informational only. Both types support management, proxy, and all configuration options. ProxyManager handles polling for all bots. Token is optional at startup.

**Multi-bot with unified table**: Single `bots` table with `manage_enabled` and `proxy_enabled` flags. Each bot can be proxy-only, management-only, or both.

**Chat isolation**: `chats` table uses compound PK `(bot_id, id)`. Messages and tags are keyed by `chat_id` (globally unique in Telegram).

**Bot resolution**: API endpoints accept `bot_id` param. Server checks registered bots map first, then ProxyManager's managed bots (`resolveBot` fallback chain).

**Bot deactivation**: Bots can be temporarily disabled via `disabled` flag (`/api/bots/toggle-disabled`). Disabled bots keep their configuration and data but stop polling, are unregistered from managed bots and webhook mode. Admin-only toggle, visually distinguished in the frontend with reduced opacity and a badge.

**Telegram API proxy** (`/tgapi/bot{TOKEN}/{method}`): Reverse-proxies backend API calls to `api.telegram.org`. For send methods (`sendMessage`, `sendPhoto`, etc.), parses the Telegram response to extract the sent `Message` and saves it to DB. This captures outgoing bot messages that don't appear in `getUpdates`.

**Webhook mode**: Bots marked via `SetWebhookMode()` are not polled by ProxyManager but still show as running via `IsRunning()`. `WebhookHandler()` supports both management processing and proxy forwarding.

**Long polling** (`/api/updates/poll`): Pull-based alternative to push proxy. When `long_poll_enabled` is set on a bot, raw Telegram updates are buffered in an in-memory `UpdateQueue` (ring buffer, max 1000 updates per bot). External clients poll `GET /api/updates/poll?bot_id=X&offset=Y&limit=Z&timeout=T`. Response format is Telegram-compatible `{"ok":true,"result":[...]}`. Waiter notification pattern wakes blocked clients on new updates. Multiple clients can poll simultaneously. Auth required (Bearer or session). Hooks into both `pollLoop` (for polled bots) and `processUpdate` (for webhook-mode bots). Works alongside push proxy — both can be active for the same bot.

**LLM routing**: `ProxyManager` has `llmRouter *LLMRouter`. `applyLLMRoutes()` runs after rule-based `applyRoutes()` in `processUpdate()`. LLM receives message + all bot descriptions/chats and returns `{target_bot_id, target_chat_id, action, reason}`. Reverse routing works via existing `route_mappings` (RouteID=0 for LLM routes). Config managed via `/api/llm-config` and `/api/llm-config/save`. Bot descriptions via `/api/bots/description`.

**Authentication**: Dual auth: cookie-based sessions (30-day expiry, HttpOnly, SameSite=Strict) and Bearer API keys (`Authorization: Bearer bmx_...`). `authMiddleware` checks Bearer token first, falls back to cookie. Passwords hashed with bcrypt, API keys with SHA-256. Two roles: `admin` (full access to all bots and settings) and `user` (access only to assigned bots). Many-to-many user↔bot via `user_bots` junction table. API keys stored in `api_keys` table, bound to users (inherit role/permissions). Default admin auto-created with `must_change_password=true`. Auth endpoints at `/api/auth/*`. API key management at `/api/auth/api-keys/*` (admin only). No auth on `/tgapi/` (backends use it), `/` (SPA handles client-side), `/api/health`. Admin-only: bot CRUD, user management, API key management, routes, LLM config. Frontend hides admin controls for regular users and handles 401 → login redirect.

**Custom Telegram API**: Package-level `telegramAPIURL` var (default `https://api.telegram.org`), set via `-tg-api` flag or `TELEGRAM_API_URL` env var. All direct HTTP calls in `proxy.go`, `server.go` use it. `bot.go` uses `NewBotAPIWithAPIEndpoint()` from the library when custom URL is set.

**Demo mode** (`-demo` flag or `DEMO_MODE=true`): Uses a separate `demo.db` database file with the same normal application flow. On first launch, `seedDemoData()` creates a `demo:demo` admin user (no password change required) and 3 demo bots. Forces `telegramAPIURL` to `https://telegram-bot-api.exe.xyz`. Server's `demoMode` flag makes `/api/health` return `{"mode":"demo"}` so frontend auto-fills credentials and shows demo hint.

**Media handling**: Messages store `media_type`, `file_id`, `from_is_bot` (whether sender is a bot), and `sender_tag` (supergroup member tag from Bot API 9.5+). `bot.go:extractMedia()` detects photo/video/animation/sticker/voice/audio/document/video_note from Telegram updates. For stickers, uses `Thumbnail.FileID` (static preview) instead of main file (which may be TGS/WebM). `server.go:captureSentMessage` extracts media from API responses. `/api/media?file_id=&bot_id=` proxies file downloads from Telegram with automatic WebP→PNG conversion via `go-webp` for browser compatibility. Frontend renders images with lightbox overlay (click to zoom), video players, audio players inline. Messages support reply-to with `reply_to_message_id` in send API and visual reply badges in the UI.

## Dependencies

- `github.com/OvyFlash/telegram-bot-api` — Telegram Bot API (actively maintained fork)
- `modernc.org/sqlite` — SQLite driver (pure Go, no CGO)
- `github.com/skrashevich/go-webp` — Pure Go WebP codec for sticker conversion (WebP→PNG)
- `golang.org/x/crypto/bcrypt` — Password hashing for authentication

## Language

Frontend supports English and Russian via i18n system in `internal/server/templates/index.html`. Translations are in the `i18n` object (keys `en`/`ru`). `t(key)` returns the current language string. `applyLang()` re-renders all static and dynamic content. Language preference stored in localStorage. Comments may be in English or Russian. README is English.

**Version checking**: Build-time variables (`version`, `commit`, `buildDate`) injected via `-ldflags "-X main.version=..."`. CI workflows (release.yml, docker.yml) and Dockerfile pass these automatically. `VersionChecker` queries GitHub Releases API (`/repos/skrashevich/botmux/releases/latest`) with 6-hour in-memory cache. Dev builds (`version=dev`) skip the check. Admin-only update banner in sidebar, dismissible per version (persisted in localStorage). `-version` CLI flag prints build info and exits.

**User profile card**: Click on any username in chat messages to open a modal with aggregated user data: identity (name, ID, admin status/title), message count, first/last seen, tags, admin permissions, and recent admin actions history. Data fetched from `/api/users/profile` endpoint which aggregates from `known_users`, `messages`, admin list, `user_tags`, and `admin_log` tables.

## Screenshots

Screenshots in `screenshots/` use redacted data (fake usernames/URLs). Regenerate with a puppeteer script if UI changes significantly. Use `evaluateOnNewDocument` to monkey-patch `fetch` for data redaction (DOM replacement doesn't work reliably with innerHTML-rendered content).
