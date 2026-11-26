package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	tele "gopkg.in/telebot.v3"
)

const memorySharingInstruction = `

If asked to share or withdraw a remembered conclusion, explain that the owner confirms in this private chat
with /memory_share <group ID> <exact conclusion or excerpt> or /memory_unshare <group ID> <exact conclusion or excerpt>.
Help identify the exact visible wording of the one conclusion or excerpt they want to share. Never display raw memory records. Do not say that you granted or withdrew sharing in a conversational reply.`

const memoryAdminUsage = "Use /memory list [cursor], /memory show <id> [part], /memory edit <id> <version> <conclusion>, or /memory delete <id> <version>."

// Telebot's command dispatcher sets Payload from the first line. Keep the
// rest of the exact text so multiline conclusions can be managed or shared.
func memoryCommandPayload(msg *tele.Message) string {
	if i := strings.IndexFunc(msg.Text, unicode.IsSpace); i >= 0 {
		return strings.TrimSpace(msg.Text[i:])
	}
	return strings.TrimSpace(msg.Payload)
}

func (b *Bot) handleMemoryAdmin(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if !ownPrivateMessage(c.Message()) || !telegramOwnContent(c.Message()) {
		return c.Reply("Memory management is available in your private admin chat.")
	}
	return b.replyAsAdmin(c, "Only admins can manage raw memory.", func(snap runtimeSnapshot) string {
		return b.memoryAdminReply(snap.memory, memoryCommandPayload(c.Message()))
	})
}

func (b *Bot) memoryAdminReply(book *MemoryBook, input string) string {
	if book == nil {
		return "Memory Book is unavailable."
	}
	action, rest, _ := strings.Cut(strings.TrimSpace(input), " ")
	rest = strings.TrimSpace(rest)
	id, tail, _ := strings.Cut(rest, " ")
	switch action {
	case "list":
		if strings.TrimSpace(tail) != "" {
			return memoryAdminUsage
		}
		const pageSize = 8
		entries, err := book.List(id, pageSize+1)
		if err != nil {
			return memoryCommandError(err)
		}
		if len(entries) == 0 {
			return "No memory entries on this page."
		}
		var out strings.Builder
		for _, entry := range entries[:min(pageSize, len(entries))] {
			fmt.Fprintf(&out, "%s v%d | chat %d | %s\n", entry.Ref.ID, entry.Ref.Version, entry.AudienceChatID, truncate(entry.Text, 200))
		}
		if len(entries) > pageSize {
			fmt.Fprintf(&out, "\nNext page: /memory list %s", entries[pageSize-1].Ref.ID)
		}
		return out.String()
	case "show":
		entry, err := book.Get(id)
		if err != nil {
			return memoryCommandError(err)
		}
		part := 1
		if strings.TrimSpace(tail) != "" {
			part, err = strconv.Atoi(strings.TrimSpace(tail))
			if err != nil || part < 1 {
				return memoryAdminUsage
			}
		}
		raw, err := json.MarshalIndent(entry, "", "  ")
		if err != nil {
			return memoryCommandError(err)
		}
		runes := []rune(string(raw))
		const chunk = 3000
		parts := (len(runes) + chunk - 1) / chunk
		if part > parts {
			return "No such part."
		}
		start := (part - 1) * chunk
		out := fmt.Sprintf("Memory %s v%d (%d/%d)\n%s", entry.Ref.ID, entry.Ref.Version, part, parts, string(runes[start:min(start+chunk, len(runes))]))
		if part < parts {
			out += fmt.Sprintf("\n\nNext part: /memory show %s %d", entry.Ref.ID, part+1)
		}
		return out
	case "edit", "delete":
		version, conclusion, _ := strings.Cut(strings.TrimSpace(tail), " ")
		n, err := strconv.ParseUint(version, 10, 64)
		if err != nil || id == "" || n == 0 {
			return memoryAdminUsage
		}
		ref := MemoryRef{ID: id, Version: n}
		conclusion = strings.TrimSpace(conclusion)
		if action == "delete" {
			if conclusion != "" {
				return memoryAdminUsage
			}
			if err := book.Delete(ref); err != nil {
				return memoryCommandError(err)
			}
			b.memoryWriteEpoch.Add(1)
			return "Memory deleted."
		}
		if conclusion == "" || len(conclusion) > 1200 {
			return memoryAdminUsage
		}
		ref, err = book.Revise(ref, MemoryEdit{Text: conclusion})
		if err != nil {
			return memoryCommandError(err)
		}
		b.memoryWriteEpoch.Add(1)
		return fmt.Sprintf("Memory %s updated to v%d.", ref.ID, ref.Version)
	default:
		return memoryAdminUsage
	}
}

func memoryCommandError(err error) string {
	switch {
	case errors.Is(err, ErrMemoryConflict):
		return "The memory changed. Check its latest version and try again."
	case errors.Is(err, ErrMemoryNotFound), errors.Is(err, ErrMemoryDenied):
		return "No matching memory is available."
	default:
		return "The memory operation could not be completed."
	}
}

func (b *Bot) handleMemoryShare(c tele.Context) error {
	return b.changeMemoryShare(c, false)
}

func (b *Bot) handleMemoryUnshare(c tele.Context) error {
	return b.changeMemoryShare(c, true)
}

func (b *Bot) changeMemoryShare(c tele.Context, revoke bool) error {
	if !b.isAllowed(c) {
		return nil
	}
	msg := c.Message()
	if !ownPrivateMessage(msg) || !telegramOwnContent(msg) {
		return c.Reply("Confirm memory sharing in your own private chat.")
	}
	group, conclusion, _ := strings.Cut(memoryCommandPayload(msg), " ")
	groupID, err := strconv.ParseInt(group, 10, 64)
	conclusion = strings.TrimSpace(conclusion)
	if err != nil || groupID >= 0 || conclusion == "" || len(conclusion) > 1200 {
		return c.Reply("Use /memory_share <group ID> <exact conclusion or excerpt> or /memory_unshare <group ID> <exact conclusion or excerpt>.")
	}

	snap := b.snapshot()
	sender := msg.Sender

	var ref MemoryRef
	var destination *tele.Chat
	if !revoke {
		ref, err = snap.memory.findOwnConclusion(sender.ID, conclusion)
		if err != nil {
			return c.Reply(memoryCommandError(err))
		}
		destination, err = snap.tg.ChatByID(groupID)
		if err != nil || !telegramGroup(destination) || destination.ID != groupID {
			return c.Reply("Telegram could not verify this group.")
		}
		if !verifyTelegramGroupMember(snap.tg, destination, sender) {
			return c.Reply("Telegram could not verify your membership in this group.")
		}
	}

	b.runtimeConfigMu.Lock()
	current := b.snapshot()
	var reply string
	switch {
	case !current.cfg.isAllowed(c):
		b.runtimeConfigMu.Unlock()
		return nil
	case !revoke && !current.cfg.allowsChat(destination, sender):
		reply = "This group is outside the bot's current access rules."
	case revoke:
		if err := current.memory.withdrawOwnConclusion(sender.ID, groupID, conclusion); err != nil {
			reply = memoryCommandError(err)
		} else {
			reply = "Memory sharing withdrawn for this group."
		}
	default:
		if _, err := current.memory.Share(ref, sender.ID, groupID, conclusion); err != nil {
			reply = memoryCommandError(err)
		} else {
			reply = "This conclusion can now be used in the specified group."
		}
	}
	b.runtimeConfigMu.Unlock()
	return c.Reply(reply)
}
