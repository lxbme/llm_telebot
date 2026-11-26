package app

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

type groupToolKey struct {
	group, owner int64
	name         string
}

type groupToolGrant struct {
	key  groupToolKey
	tool *MCPRemoteTool
	args string
}

const groupToolUsage = "In your private chat, use /mcp_share <group ID> <tool name> <JSON arguments> to authorize this exact network lookup and its result for the group. In that group, request it with /mcp_use <tool name> <JSON arguments>. Use /mcp_unshare <group ID> <tool name> in private to withdraw it."

func groupToolArguments(input string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	var args map[string]any
	if decoder.Decode(&args) != nil || args == nil || decoder.Decode(new(any)) != io.EOF {
		return "", false
	}
	raw, err := json.Marshal(args)
	return string(raw), err == nil
}

func (b *Bot) handleMCPShare(c tele.Context) error {
	return b.changeGroupToolGrant(c, false)
}

func (b *Bot) handleMCPUnshare(c tele.Context) error {
	return b.changeGroupToolGrant(c, true)
}

func (b *Bot) changeGroupToolGrant(c tele.Context, revoke bool) error {
	if !b.isAllowed(c) {
		return nil
	}
	if !ownPrivateMessage(c.Message()) || !telegramOwnContent(c.Message()) {
		return c.Reply(groupToolUsage)
	}
	group, rest, _ := strings.Cut(memoryCommandPayload(c.Message()), " ")
	name, raw, _ := strings.Cut(strings.TrimSpace(rest), " ")
	groupID, err := strconv.ParseInt(group, 10, 64)
	if err != nil || groupID >= 0 || name == "" || name == getToolDetailsVirtualTool.Function.Name {
		return c.Reply(groupToolUsage)
	}
	owner := c.Sender()
	key := groupToolKey{group: groupID, owner: owner.ID, name: name}
	if revoke {
		if raw != "" {
			return c.Reply(groupToolUsage)
		}
		b.groupToolGrants.Delete(key)
		return c.Reply("Tool authorization withdrawn for this group.")
	}
	args, ok := groupToolArguments(raw)
	if !ok {
		return c.Reply(groupToolUsage)
	}
	snap := b.snapshot()
	tool := personalNetworkTool(snap.userTools, key)
	if tool == nil {
		return c.Reply("No matching personal network tool is connected.")
	}
	chat, err := snap.tg.ChatByID(groupID)
	if err != nil || !telegramGroup(chat) || chat.ID != groupID || !verifyTelegramGroupMember(snap.tg, chat, owner) {
		return c.Reply("Telegram could not verify your membership in this group.")
	}
	b.runtimeConfigMu.Lock()
	current := b.snapshot()
	allowed := current.cfg.allowsToolGrant(chat, owner) &&
		personalNetworkTool(current.userTools, key) == tool
	if allowed {
		b.groupToolGrants.Store(key, &groupToolGrant{key: key, tool: tool, args: args})
	}
	b.runtimeConfigMu.Unlock()
	if !allowed {
		return c.Reply("The selected group or tool is no longer available.")
	}
	return c.Reply("This lookup is authorized. In that group, request it with /mcp_use <tool name> <JSON arguments>.")
}

func (b *Bot) handleMCPUse(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if !telegramGroup(c.Chat()) {
		return c.Reply(groupToolUsage)
	}
	msg := c.Message()
	turn := b.beginTelegramTurn(msg)
	name, raw, _ := strings.Cut(memoryCommandPayload(msg), " ")
	args, ok := groupToolArguments(raw)
	if !turn.Source.valid() || !ok {
		return turn.reply(c, groupToolUsage)
	}
	key := groupToolKey{group: turn.ChatID, owner: turn.SenderID, name: name}
	value, _ := b.groupToolGrants.Load(key)
	grant, _ := value.(*groupToolGrant)
	if grant == nil || grant.args != args {
		return turn.reply(c, "There is no matching authorization for this lookup in this group.")
	}
	turn.groupTool = grant
	if !turn.groupToolValid(b.snapshot()) {
		return turn.reply(c, "The selected tool is no longer available.")
	}
	if denial, allowed := b.checkDailyQuota(turn.BillingUserID, turn.ChatID); !allowed {
		return turn.reply(c, denial)
	}
	userMsg := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Name:    sanitizeName(turn.SenderID),
		Content: buildUserContent(msg.Sender, "Run "+name+" with these exact JSON arguments: "+args),
	}
	return b.startRoleReply(turn, c.Chat(), msg.Sender, userMsg, true, "", "")
}

func personalNetworkTool(manager *UserToolManager, key groupToolKey) *MCPRemoteTool {
	if manager == nil {
		return nil
	}
	registry := manager.GetRegistry(key.owner)
	if registry == nil {
		return nil
	}
	remote, _ := registry.Get(key.name).(*MCPRemoteTool)
	if remote == nil || remote.command != nil {
		return nil
	}
	return remote
}

func (b *Bot) roleReplyTools(turn RoleTurn, snap runtimeSnapshot) *MergedToolView {
	if snap.tools == nil {
		return nil
	}
	var personal *ToolRegistry
	switch {
	case !turn.telegram || !turn.interactive || turn.SenderID <= 0 || snap.userTools == nil:
	case turn.chatType == tele.ChatPrivate && turn.ChatID == turn.SenderID && turn.ToolOwnerID == turn.SenderID:
		personal = snap.userTools.GetRegistry(turn.SenderID)
	case turn.groupTool != nil:
		grant := turn.groupTool
		// The personal description, server name and schema may describe
		// data outside this group. Expose only a generic declaration.
		scoped := *grant.tool
		scoped.serverName = "personal_" + grant.key.name
		scoped.tool = mcp.NewToolWithRawSchema(grant.key.name,
			"Look up the specific data the requesting account authorized for this group. Supply only the exact arguments requested by that account.",
			[]byte(`{"type":"object","additionalProperties":true}`))
		personal = NewToolRegistry()
		personal.Register(&scoped)
	}
	return NewMergedToolView(snap.tools, personal)
}

func (turn RoleTurn) groupToolValid(snap runtimeSnapshot) bool {
	grant := turn.groupTool
	if grant == nil {
		return true
	}
	chat := &tele.Chat{ID: turn.ChatID, Type: turn.chatType}
	current, _ := turn.bot.groupToolGrants.Load(grant.key)
	return turn.interactive && turn.telegram && turn.Source.valid() && telegramGroup(chat) &&
		grant.key.group == turn.ChatID && grant.key.owner == turn.SenderID &&
		snap.tools != nil && current == grant && personalNetworkTool(snap.userTools, grant.key) == grant.tool &&
		snap.cfg.allowsToolGrant(chat, &tele.User{ID: turn.SenderID})
}

func (cfg Config) allowsToolGrant(group *tele.Chat, owner *tele.User) bool {
	return cfg.allowsChat(&tele.Chat{ID: owner.ID, Type: tele.ChatPrivate}, owner) && cfg.allowsChat(group, owner)
}
