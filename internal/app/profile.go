package app

import (
	"fmt"
	"log"
	"strings"

	tele "gopkg.in/telebot.v3"
)

// The legacy profile store is exposed only through its existing management
// paths. Role prompts and new learning use the audience-scoped Memory Book.

func formatProfileIdentity(userID int64, username string) string {
	username = strings.TrimSpace(username)
	if username != "" {
		return fmt.Sprintf("@%s (%d)", username, userID)
	}
	return fmt.Sprintf("user_%d", userID)
}

func (b *Bot) handleClearProfile(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if !ownPrivateMessage(c.Message()) {
		return c.Reply("Your legacy profile is available in your own private chat.")
	}
	userID := c.Sender().ID
	snap := b.snapshot()
	if snap.profiles == nil {
		return c.Reply("⚠️ The legacy profile store is disabled.")
	}
	if err := snap.profiles.Delete(userID); err != nil {
		log.Printf("[profile] delete error for user %d: %v", userID, err)
		return c.Reply("❌ Failed to clear profile.")
	}
	return c.Reply("✅ Your user profile has been cleared.")
}

func (b *Bot) handleDisplayProfile(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if !ownPrivateMessage(c.Message()) {
		return c.Reply("Your legacy profile is available in your own private chat.")
	}
	userID := c.Sender().ID
	snap := b.snapshot()
	if snap.profiles == nil {
		return c.Reply("⚠️ The legacy profile store is disabled.")
	}
	profile, err := snap.profiles.Get(userID)
	if err != nil {
		log.Printf("[profile] read error for user %d: %v", userID, err)
		return c.Reply("❌ Failed to read profile.")
	}
	if profile == nil || len(profile.Facts) == 0 {
		return c.Reply("📭 No legacy profile data.")
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👤 Profile for %s:\n", formatProfileIdentity(userID, profile.Username)))
	for i, fact := range profile.Facts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, fact))
	}
	if profile.DisplayName != "" {
		sb.WriteString(fmt.Sprintf("\n🪪 Display name: %s", profile.DisplayName))
	}
	sb.WriteString(fmt.Sprintf("\n🕐 Last updated: %s", profile.UpdatedAt.Format("2006-01-02 15:04 UTC")))
	return c.Reply(sb.String())
}
