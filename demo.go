package main

import (
	"log"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

// seedDemoData populates a fresh database with demo user and bots.
// Called once on startup in demo mode if no bots exist yet.
func seedDemoData(st *store.Store) {
	// Check if data already exists
	bots, _ := st.GetBotConfigs()
	if len(bots) > 0 {
		log.Printf("[demo] Database already has %d bot(s), skipping seed", len(bots))
		return
	}

	log.Printf("[demo] Seeding demo data...")

	// Update default admin to demo:demo with no password change
	hash, err := auth.HashPassword("demo")
	if err != nil {
		log.Printf("[demo] Failed to hash password: %v", err)
		return
	}
	if err := st.UpdateDemoAdmin(hash); err != nil {
		log.Printf("[demo] Failed to update admin user: %v", err)
		return
	}

	// Seed demo bots
	demoBots := []models.BotConfig{
		{
			Name:          "Support Bot",
			Token:         "111111111:AAFakeToken_SupportBot_Demo",
			ManageEnabled: true,
		},
		{
			Name:          "News Bot",
			Token:         "222222222:AAFakeToken_NewsBot_Demo",
			ManageEnabled: true,
		},
		{
			Name:          "Moderation Bot",
			Token:         "333333333:AAFakeToken_ModerationBot_Demo",
			ManageEnabled: true,
		},
	}

	for _, b := range demoBots {
		if _, err := st.AddBotConfig(b); err != nil {
			log.Printf("[demo] Failed to add bot %s: %v", b.Name, err)
		}
	}

	log.Printf("[demo] Seeded %d demo bots", len(demoBots))
}
