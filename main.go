package main

import (
	"flag"
	"log"
	"os"
)

func main() {
	token := flag.String("token", "", "Telegram bot token")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "botdata.db", "SQLite database path")
	webhookURL := flag.String("webhook", "", "Set webhook URL (e.g. https://example.com/tghook). If empty, auto-detects mode.")
	forcePolling := flag.Bool("polling", false, "Force polling mode (removes any existing webhook!)")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if *token == "" {
		log.Fatal("Bot token required: use -token flag or TELEGRAM_BOT_TOKEN env var")
	}

	store, err := NewStore(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	// Start proxy manager (handles all non-CLI bots)
	proxy := NewProxyManager(store)

	// Register CLI bot in the bots table and get its ID
	// We need a temporary bot to get the username first
	cliBot, err := NewBot(*token, store, 0) // temporary botID=0
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	botID, err := store.RegisterCLIBot(*token, cliBot.GetBotInfo())
	if err != nil {
		log.Fatalf("Failed to register CLI bot: %v", err)
	}
	cliBot.botID = botID // set the real botID

	// Migrate legacy chats (bot_id=0 -> real botID)
	store.MigrateLegacyChats(botID)

	// Determine update reception mode for CLI bot
	mode := "management-only"
	webhookPath := ""

	if *forcePolling {
		mode = "polling"
		log.Println("Mode: forced long polling (will remove existing webhook)")
	} else if *webhookURL != "" {
		mode = "webhook"
		webhookPath = "/tghook"
		if err := cliBot.SetWebhook(*webhookURL); err != nil {
			log.Fatalf("Failed to set webhook: %v", err)
		}
		log.Printf("Mode: webhook at %s", *webhookURL)
	} else {
		status, err := cliBot.CheckWebhook()
		if err != nil {
			log.Printf("Warning: could not check webhook status: %v", err)
			log.Println("Falling back to polling mode")
			mode = "polling"
		} else if status.HasWebhook {
			log.Printf("Mode: management-only (existing webhook detected: %s)", status.URL)
			log.Println("  Updates will NOT be received — another service owns the webhook.")
			log.Println("  Use -webhook <url> to set your own, or -polling to force polling.")
			if status.PendingUpdates > 0 {
				log.Printf("  (%d pending updates on Telegram side)", status.PendingUpdates)
			}
		} else {
			mode = "polling"
			log.Println("Mode: long polling (no webhook detected)")
		}
	}

	switch mode {
	case "polling":
		go cliBot.StartPolling()
	case "webhook":
		// webhook handler will be registered with the server
	case "management-only":
		log.Println("Bot API calls (send, ban, pin, etc.) will work. Message tracking is disabled.")
	}

	// Start proxy manager for non-CLI bots
	proxy.Start()
	defer proxy.StopAll()

	// Create server and register CLI bot
	server := NewServer(store, proxy)
	server.RegisterBot(botID, cliBot)

	if mode == "webhook" {
		server.SetWebhookHandler(webhookPath, cliBot.WebhookHandler())
	}

	if err := server.Start(*addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
