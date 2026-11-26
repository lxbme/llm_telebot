package app

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v3"
)

func TestTelegramEditErrorsRedactBotToken(t *testing.T) {
	server := httptest.NewServer(nil)
	server.Close()
	const token = "123456:test-token-for-edit"
	bot, err := tele.NewBot(tele.Settings{Token: token, URL: server.URL, Client: server.Client(), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	snap := runtimeSnapshot{tg: bot}
	snap.editFinalResponse(&tele.Message{ID: 1, Chat: &tele.Chat{ID: 10}}, "**hello**", func() bool { return true })
	log.SetOutput(previous)
	if logged := output.String(); strings.Contains(logged, token) || !strings.Contains(logged, "connection refused") {
		t.Fatal("Telegram edit error must keep the diagnosis and redact the token")
	}
}
