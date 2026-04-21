package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/bridge"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
	verpkg "github.com/skrashevich/botmux/internal/version"
	"github.com/skrashevich/botmux/pkg/logbuf"
)

// Build-time variables injected via ldflags
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// telegramAPIURL is the base URL for Telegram Bot API requests.
// Override with -tg-api flag or TELEGRAM_API_URL env var for testing or local Bot API server.
var telegramAPIURL = "https://api.telegram.org"

// @title BotMux API
// @version 1.0
// @description Multi-bot Telegram manager with proxying, routing and LLM-based message dispatch.
// @contact.name BotMux
// @license.name MIT
// @host localhost:8080
// @BasePath /
// @securityDefinitions.apikey CookieAuth
// @in cookie
// @name botmux_session
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description API key authentication. Use "Bearer bmx_..." format.
func main() {
	token := flag.String("token", "", "Telegram bot token (optional if bots already exist in DB)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "botdata.db", "SQLite database path")
	webhookURL := flag.String("webhook", "", "Set webhook URL for the CLI bot (requires -token)")
	tgAPI := flag.String("tg-api", "", "Custom Telegram API base URL (default: https://api.telegram.org)")
	demoMode := flag.Bool("demo", false, "Enable demo mode with separate database and seeded data")
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("botmux %s (commit: %s, built: %s)\n", version, commit, buildDate)
		os.Exit(0)
	}

	if *token == "" {
		*token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}

	if *tgAPI == "" {
		*tgAPI = os.Getenv("TELEGRAM_API_URL")
	}
	if *tgAPI != "" {
		telegramAPIURL = strings.TrimRight(*tgAPI, "/")
		log.Printf("Using custom Telegram API: %s", telegramAPIURL)
	}

	if !*demoMode && os.Getenv("DEMO_MODE") == "true" {
		*demoMode = true
	}

	// Demo mode: separate database, fake Telegram API, seeded data
	if *demoMode {
		telegramAPIURL = "https://telegram-bot-api.exe.xyz"
		log.Printf("Demo mode enabled. Telegram API: %s", telegramAPIURL)
		log.Printf("Login with demo:demo")
		*dbPath = "demo.db"
	}

	// Set up log buffer to capture application logs for web UI
	logBuf := logbuf.New(1000)
	log.SetOutput(io.MultiWriter(os.Stderr, logBuf))

	st, err := store.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer st.Close()

	if *demoMode {
		seedDemoData(st)
	}

	pm := proxy.NewManager(st, telegramAPIURL)
	srv := server.NewServer(st, pm)
	srv.DemoMode = *demoMode
	srv.LogBuf = logBuf
	srv.VersionChecker = verpkg.NewChecker(version, commit, buildDate)
	srv.TgAPIBaseURL = telegramAPIURL

	// Register CLI bot if token is provided
	if *token != "" {
		cliBot, err := bot.NewBot(*token, st, 0, telegramAPIURL)
		if err != nil {
			log.Fatalf("Failed to create bot: %v", err)
		}

		botID, err := st.RegisterCLIBot(*token, cliBot.GetBotInfo())
		if err != nil {
			log.Fatalf("Failed to register CLI bot: %v", err)
		}
		cliBot.SetBotID(botID)

		st.MigrateLegacyChats(botID)
		pm.RegisterManagedBot(botID, cliBot)
		srv.RegisterBot(botID, cliBot)

		if *webhookURL != "" {
			if err := cliBot.SetWebhook(*webhookURL); err != nil {
				log.Fatalf("Failed to set webhook: %v", err)
			}
			pm.SetWebhookMode(botID)
			srv.SetWebhookHandler("/tghook", pm.WebhookHandler(botID))
			log.Printf("CLI bot [%d] @%s: webhook mode at %s", botID, cliBot.GetBotInfo(), *webhookURL)
		} else {
			if err := pm.DeleteWebhook(*token); err != nil {
				log.Printf("Warning: could not delete webhook: %v", err)
			}
			log.Printf("CLI bot [%d] @%s: polling mode", botID, cliBot.GetBotInfo())
		}
	} else if !*demoMode {
		bots, _ := st.GetBotConfigs()
		if len(bots) == 0 {
			log.Fatal("No token provided and no bots in database. Use -token flag or TELEGRAM_BOT_TOKEN env var to add the first bot, or add one via the web UI.")
		}
		log.Printf("No token provided, using %d bot(s) from database", len(bots))
	}

	// Start ProxyManager for ALL bots
	pm.Start()
	defer pm.StopAll()

	// Start BridgeManager
	bridgeMgr := bridge.NewManager(st, pm, telegramAPIURL)
	bridgeMgr.Start()
	srv.SetBridgeManager(bridgeMgr)

	// Set bridge notification hook on all managed bots
	bridgeMgr.InstallHooks()

	// Graceful shutdown: SIGINT/SIGTERM triggers srv.Shutdown(15s).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.StartContext(ctx, *addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
	log.Printf("shutdown: complete")
}
