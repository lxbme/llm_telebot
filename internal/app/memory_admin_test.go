package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestMemoryAdmin(t *testing.T) {
	model := newEinoReplyProvider(t, func(http.ResponseWriter, *http.Request, int, openai.ChatCompletionRequest) {
		t.Error("raw management invoked the model")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.AdminIDs = map[int64]bool{42: true}
	ref, err := b.memory.Learn(MemoryLearn{Subjects: []int64{43}, Text: "old conclusion",
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: 43, SenderID: 43, MessageID: 1}, Text: "raw evidence"}})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise Telegram command dispatch so command suffixes and actual payload
	// parsing travel through the same route as production updates.
	b.registerHandlers()
	command := func(chat, user int64, text string) {
		t.Helper()
		b.tg.ProcessUpdate(tele.Update{Message: roleMessage(chat, user, 99, "/memory "+text)})
	}
	command(-100123, 42, "show "+ref.ID)
	if got := roleReceive(t, deliveries); strings.Contains(got.text, "raw evidence") {
		t.Fatal("group received raw evidence")
	}
	command(43, 43, "show "+ref.ID)
	if got := roleReceive(t, deliveries); strings.Contains(got.text, "raw evidence") {
		t.Fatal("ordinary user received raw evidence")
	}
	command(42, 42, "list")
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, ref.ID) {
		t.Fatal("admin list did not return the entry")
	}
	command(42, 42, "show "+ref.ID)
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "raw evidence") || got.chatID != 42 {
		t.Fatalf("admin show = %+v", got)
	}
	command(42, 42, fmt.Sprintf("edit %s %d current conclusion", ref.ID, ref.Version))
	roleReceive(t, deliveries)
	current, err := b.memory.Get(ref.ID)
	if err != nil || current.Text != "current conclusion" {
		t.Fatalf("admin edit = %+v, %v", current, err)
	}
	command(42, 42, fmt.Sprintf("delete %s %d", ref.ID, ref.Version))
	roleReceive(t, deliveries)
	if _, err := b.memory.Get(ref.ID); err != nil {
		t.Fatal("stale revision deleted the entry")
	}
	cfg := b.currentConfig()
	cfg.AdminWildcard = true
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	command(42, 42, fmt.Sprintf("delete %s %d", ref.ID, current.Ref.Version))
	roleReceive(t, deliveries)
	if _, err := b.memory.Get(ref.ID); err != nil {
		t.Fatal("revoked admin deleted the entry")
	}
	cfg.AdminWildcard = false
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	command(42, 42, fmt.Sprintf("delete %s %d", ref.ID, current.Ref.Version))
	roleReceive(t, deliveries)
	if _, err := b.memory.Get(ref.ID); err != ErrMemoryNotFound {
		t.Fatalf("delete error = %v", err)
	}
}

func TestMemoryAdminLegacyProfileCommandsArePrivate(t *testing.T) {
	model := newEinoReplyProvider(t, func(http.ResponseWriter, *http.Request, int, openai.ChatCompletionRequest) {
		t.Error("legacy profile commands invoked the model")
	})
	b, deliveries := newRoleTestBot(t, model)
	if err := b.profiles.Save(&UserProfile{UserID: 42, Facts: []string{"legacy private fact"}}); err != nil {
		t.Fatal(err)
	}
	b.registerHandlers()
	command := func(chat, user int64, text string) roleTestDelivery {
		t.Helper()
		b.tg.ProcessUpdate(tele.Update{Message: roleMessage(chat, user, 99, text)})
		return roleReceive(t, deliveries)
	}
	if got := command(-100123, 42, "/displayp"); strings.Contains(got.text, "legacy private fact") {
		t.Fatal("group received the legacy profile")
	}
	command(-100123, 42, "/clearp")
	command(43, 43, "/clearp 42")
	if got := command(42, 42, "/displayp"); !strings.Contains(got.text, "legacy private fact") {
		t.Fatalf("owner could not see unchanged legacy profile: %+v", got)
	}
	command(42, 42, "/clearp")
	if got := command(42, 42, "/displayp"); !strings.Contains(got.text, "No legacy profile data") {
		t.Fatalf("legacy profile was not removed: %+v", got)
	}
}
