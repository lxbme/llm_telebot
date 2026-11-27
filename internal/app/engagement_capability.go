package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	engagementReleaseList     = "release.list"
	engagementEventList       = "event.list"
	engagementCalendarCreate  = "calendar.create"
	engagementPrivateFollowup = "private.followup"
)

// EngagementMCPPolicy is enrolled by the deployment (global observations) or
// by the authenticated owner importing a personal connector in their private
// chat (calendar creation). The MCP server's metadata alone grants nothing.
type EngagementMCPPolicy struct {
	Operation string          `json:"operation"`
	Target    string          `json:"target"`
	Arguments json.RawMessage `json:"arguments"`
	// The enrollment explicitly guarantees that the calendar tool deduplicates
	// identical writes by idempotencyKey, including after a lost response.
	Idempotent bool `json:"idempotent,omitempty"`
}

type engagementMCPBinding struct {
	EngagementMCPPolicy
	server, key string
	legacyKey   string
	config      MCPServerConfig
	schema      json.RawMessage
}

func newEngagementBinding(server string, cfg MCPServerConfig, tool mcp.Tool) *engagementMCPBinding {
	operation, ok := cfg.Background[tool.Name]
	if !ok || cfg.BackgroundIdentity == "" || IsCommandMCP(cfg) || cfg.Disabled ||
		cfg.URL == "" || strings.TrimSpace(operation.Target) == "" || len(operation.Target) > 250 {
		return nil
	}
	args, ok := groupToolArguments(string(operation.Arguments))
	contract := tool.RawInputSchema
	if len(contract) == 0 {
		contract, _ = json.Marshal(tool.InputSchema)
	}
	if !ok || len(args) > 4096 || !supportsEngagementParameters(contract, args, operation.Operation, operation.Idempotent) {
		return nil
	}
	switch operation.Operation {
	case engagementReleaseList, engagementEventList:
		if operation.Idempotent {
			return nil
		}
		if hint := tool.Annotations.ReadOnlyHint; hint != nil && !*hint {
			return nil
		}
	case engagementCalendarCreate:
		var fixed map[string]string
		if json.Unmarshal([]byte(args), &fixed) != nil || len(fixed) != 1 || fixed["calendarId"] != operation.Target {
			return nil
		}
		if hint := tool.Annotations.ReadOnlyHint; hint != nil && *hint {
			return nil
		}
	default:
		return nil
	}
	operation.Arguments = json.RawMessage(args)
	var schema any
	if decodeEngagementJSON(contract, &schema) != nil {
		return nil
	}
	// Include the configured identity, transport, credential binding and
	// discovered parameter contract. Hashes stay in trusted application data.
	identityFields := []any{cfg.BackgroundIdentity, server, strings.ToLower(InferTransportType(&cfg)),
		cfg.URL, cfg.Headers, tool.Name}
	identity, err := json.Marshal(append(identityFields, canonicalEngagementJSON(schema), operation))
	if err != nil {
		return nil
	}

	projected, _ := json.Marshal(tool.InputSchema)
	var fullSchema any
	if !sameEngagementSchema(contract, projected) {
		fullSchema = schema
	}
	var legacyKey string
	if unambiguousLegacyEngagementSchema(contract, schema, fullSchema) {
		if previous, err := json.Marshal(append(identityFields, tool.InputSchema, fullSchema, operation)); err == nil {
			legacyKey = fmt.Sprintf("engagement:mcp:%x", sha256.Sum256(previous))
		}
	}
	return &engagementMCPBinding{
		EngagementMCPPolicy: operation, server: server,
		key: fmt.Sprintf("engagement:mcp:v2:%x", sha256.Sum256(identity)), legacyKey: legacyKey, config: cfg, schema: contract,
	}
}

var engagementMCPRequestID atomic.Uint64

// The SDK's typed list decodes only a subset of the input schema. Read the
// original schema over the same initialized MCP transport for enrolled tools.
func listEngagementMCPTools(ctx context.Context, c *client.Client) ([]mcp.Tool, error) {
	var tools []mcp.Tool
	var request mcp.ListToolsRequest
	seen := make(map[mcp.Cursor]bool)
	for {
		response, err := c.GetTransport().SendRequest(ctx, transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      mcp.NewRequestId(fmt.Sprintf("engagement-tools-%d", engagementMCPRequestID.Add(1))),
			Method:  "tools/list", Params: request.Params,
		})
		if err != nil {
			return nil, err
		}
		if response.Error != nil {
			return nil, response.Error.AsError()
		}
		var page struct {
			NextCursor mcp.Cursor
			Tools      []struct {
				mcp.Tool
				Schema json.RawMessage `json:"inputSchema"`
			}
		}
		if err := json.Unmarshal(response.Result, &page); err != nil {
			return nil, err
		}
		for _, row := range page.Tools {
			row.Tool.RawInputSchema = row.Schema
			if err := decodeEngagementJSON(row.Schema, &row.Tool.InputSchema); err != nil {
				return nil, err
			}
			tools = append(tools, row.Tool)
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		if seen[page.NextCursor] {
			return nil, ErrEngagementInvalid
		}
		seen[page.NextCursor] = true
		request.Params.Cursor = page.NextCursor
	}
}

// All callers supply a JSON value from a typed protocol field, the checked
// configuration, or a generated payload. Keep numeric tokens intact.
func decodeEngagementJSON(raw []byte, result any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrEngagementInvalid
	}
	return nil
}

func supportsEngagementParameters(contract json.RawMessage, raw, operation string, idempotent bool) bool {
	var arguments map[string]any
	if decodeEngagementJSON([]byte(raw), &arguments) != nil || len(arguments) == 0 {
		return false
	}
	var schema map[string]any
	if decodeEngagementJSON(contract, &schema) != nil {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	if operation == engagementCalendarCreate {
		fields := map[string]string{"title": "Review", "start": "2026-01-01T00:00:00Z", "end": "2026-01-01T01:00:00Z"}
		if idempotent {
			fields["idempotencyKey"] = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		}
		for field, example := range fields {
			prop, ok := properties[field].(map[string]any)
			if !ok || prop["type"] != "string" {
				return false
			}
			arguments[field] = example
		}
		if prop, ok := properties["calendarId"].(map[string]any); !ok || prop["type"] != "string" {
			return false
		}
	}
	// Enrollment binds declared target parameters, even when the server
	// nominally permits arbitrary extra properties.
	for key := range arguments {
		if _, declared := properties[key]; !declared {
			return false
		}
	}
	return engagementParameterFits(schema, arguments)
}

// Accept the supported JSON Schema subset and reject unknown constraints on
// values we will send. Particularly, a schema reference or composition cannot
// silently turn an incompatible tool into a background capability.
func engagementParameterFits(schema any, value any) bool {
	switch schema := schema.(type) {
	case bool:
		return schema
	case map[string]any:
		if kind, specified := schema["type"]; specified && !engagementParameterTypeFits(kind, value) {
			return false
		}
		for key, constraint := range schema {
			switch key {
			case "$schema", "$id", "$defs", "definitions", "type", "title", "description", "default", "examples", "deprecated", "readOnly", "writeOnly":
			case "properties", "required", "additionalProperties", "items":
				// Checked below against the relevant container.
			case "enum":
				values, _ := constraint.([]any)
				if !slices.ContainsFunc(values, func(x any) bool { return equalEngagementJSON(x, value) }) {
					return false
				}
			case "const":
				if !equalEngagementJSON(constraint, value) {
					return false
				}
			case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf":
				if number, ok := value.(json.Number); ok && !engagementNumberFits(key, constraint, number) {
					return false
				}
			case "pattern", "format", "minLength", "maxLength":
				if str, ok := value.(string); ok && !engagementStringFits(key, constraint, str) {
					return false
				}
			default:
				return false
			}
		}
		// Container keywords apply to matching instances even when the
		// contract omits a type.
		switch value := value.(type) {
		case map[string]any:
			return engagementObjectFits(schema, value)
		case []any:
			if items, specified := schema["items"]; specified {
				return !slices.ContainsFunc(value, func(item any) bool { return !engagementParameterFits(items, item) })
			}
		}
		return true
	}
	return false
}

func engagementParameterTypeFits(kind, value any) bool {
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number", "integer":
		number := engagementNumber(value)
		return number != nil && (kind != "integer" || number.IsInt())
	case "null":
		return value == nil
	default:
		return false
	}
}

// Canonical JSON numbers use one decimal coefficient and exponent, without
// rounding or expanding exponents. The input has already been JSON decoded.
func canonicalEngagementNumber(number json.Number) (json.Number, bool) {
	raw := string(number)
	const limit = 4096
	if len(raw) > limit {
		return number, false
	}
	var exponent int
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exp, err := strconv.Atoi(raw[i+1:])
		if err != nil || exp < -limit || exp > limit {
			return number, false
		}
		exponent, raw = exp, raw[:i]
	}
	sign := ""
	if unsigned, negative := strings.CutPrefix(raw, "-"); negative {
		sign, raw = "-", unsigned
	}
	if whole, fraction, ok := strings.Cut(raw, "."); ok {
		exponent -= len(fraction)
		raw = whole + fraction
	}
	digits := strings.TrimLeft(raw, "0")
	if digits == "" {
		return "0", true
	}
	coefficient := strings.TrimRight(digits, "0")
	exponent += len(digits) - len(coefficient)
	if exponent == 0 {
		return json.Number(sign + coefficient), true
	}
	return json.Number(sign + coefficient + "e" + strconv.Itoa(exponent)), true
}

func canonicalEngagementJSON(value any) any {
	switch value := value.(type) {
	case json.Number:
		canonical, _ := canonicalEngagementNumber(value)
		return canonical
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			result[key] = canonicalEngagementJSON(child)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = canonicalEngagementJSON(child)
		}
		return result
	default:
		return value
	}
}

func engagementNumber(value any) *big.Rat {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	canonical, bounded := canonicalEngagementNumber(number)
	if !bounded {
		return nil
	}
	exact, _ := new(big.Rat).SetString(string(canonical))
	return exact
}

func engagementNumberFits(key string, constraint any, value json.Number) bool {
	number, limit := engagementNumber(value), engagementNumber(constraint)
	if number == nil || limit == nil {
		return false
	}
	switch key {
	case "minimum":
		return number.Cmp(limit) >= 0
	case "maximum":
		return number.Cmp(limit) <= 0
	case "exclusiveMinimum":
		return number.Cmp(limit) > 0
	case "exclusiveMaximum":
		return number.Cmp(limit) < 0
	case "multipleOf":
		return limit.Sign() > 0 && number.Quo(number, limit).IsInt()
	}
	return false
}

func equalEngagementJSON(left, right any) bool {
	switch left := left.(type) {
	case json.Number:
		r, ok := right.(json.Number)
		if !ok {
			return false
		}
		a, _ := canonicalEngagementNumber(left)
		b, _ := canonicalEngagementNumber(r)
		return a == b
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(left) != len(r) {
			return false
		}
		for key, value := range left {
			other, ok := r[key]
			if !ok || !equalEngagementJSON(value, other) {
				return false
			}
		}
		return true
	case []any:
		r, ok := right.([]any)
		return ok && slices.EqualFunc(left, r, equalEngagementJSON)
	default:
		return left == right
	}
}

func engagementObjectFits(schema, object map[string]any) bool {
	if raw, specified := schema["required"]; specified {
		required, ok := raw.([]any)
		if !ok {
			return false
		}
		for _, key := range required {
			name, ok := key.(string)
			if _, exists := object[name]; !ok || !exists {
				return false
			}
		}
	}
	properties, ok := schema["properties"].(map[string]any)
	if _, specified := schema["properties"]; specified && !ok {
		return false
	}
	for key, item := range object {
		prop, specified := properties[key]
		if !specified {
			prop, specified = schema["additionalProperties"]
		}
		if specified && !engagementParameterFits(prop, item) {
			return false
		}
	}
	return true
}

func engagementStringFits(key string, constraint any, str string) bool {
	switch key {
	case "pattern":
		pattern, ok := constraint.(string)
		matched, err := regexp.MatchString(pattern, str)
		return ok && err == nil && matched
	case "format":
		switch constraint {
		case "date-time":
			_, err := time.Parse(time.RFC3339Nano, str)
			return err == nil
		default:
			return false
		}
	case "minLength", "maxLength":
		limit := engagementNumber(constraint)
		length := new(big.Rat).SetInt64(int64(utf8.RuneCountInString(str)))
		return limit != nil && limit.IsInt() && limit.Sign() >= 0 &&
			(key == "minLength" && length.Cmp(limit) >= 0 || key == "maxLength" && length.Cmp(limit) <= 0)
	}
	return false
}

type resolvedEngagementCapability struct {
	tool *MCPRemoteTool
}

type engagementCalendarEvent struct {
	Title      string
	Start, End time.Time
}

// arguments uses the pinned observation query or builds the only supported
// calendar operation. A runner supplies the event verified against the
// agreement's observation and trigger. There is no arbitrary payload path.
func (r resolvedEngagementCapability) arguments(event *engagementCalendarEvent) (string, error) {
	binding := r.tool.background
	if binding.Operation != engagementCalendarCreate {
		if event != nil {
			return "", ErrEngagementInvalid
		}
		return string(binding.Arguments), nil
	}
	raw, err := calendarCreateArgs(binding.Target, event)
	if err != nil || binding.Idempotent {
		// An idempotent request gets its key only after reservation.
		return raw, err
	}
	return r.withIdempotencyKey(raw, "")
}

func calendarCreateArgs(target string, event *engagementCalendarEvent) (string, error) {
	if event == nil || strings.TrimSpace(event.Title) == "" || len(event.Title) > 512 ||
		event.Start.IsZero() || !event.End.After(event.Start) {
		return "", ErrEngagementInvalid
	}
	args := struct {
		CalendarID string    `json:"calendarId"`
		Title      string    `json:"title"`
		Start      time.Time `json:"start"`
		End        time.Time `json:"end"`
	}{target, event.Title, event.Start, event.End}
	raw, err := json.Marshal(args)
	return string(raw), err
}

func (r resolvedEngagementCapability) withIdempotencyKey(raw, key string) (string, error) {
	binding := r.tool.background
	var values, contract map[string]any
	if decodeEngagementJSON([]byte(raw), &values) != nil || decodeEngagementJSON(binding.schema, &contract) != nil {
		return "", ErrEngagementInvalid
	}
	if binding.Idempotent {
		if key == "" {
			return "", ErrEngagementInvalid
		}
		values["idempotencyKey"] = key
	}
	if !engagementParameterFits(contract, values) {
		return "", ErrEngagementInvalid
	}
	args, err := json.Marshal(values)
	return string(args), err
}

func (r resolvedEngagementCapability) verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tools, err := listEngagementMCPTools(ctx, r.tool.client)
	if err != nil {
		return err
	}
	return r.matchDiscovered(tools)
}

func (r resolvedEngagementCapability) matchDiscovered(tools []mcp.Tool) error {
	pinned := r.tool.background
	var found bool
	for _, tool := range tools {
		if tool.Name != r.tool.Name() {
			continue
		}
		live := newEngagementBinding(pinned.server, pinned.config, tool)
		if found || live == nil || live.key != pinned.key {
			return ErrEngagementDenied
		}
		found = true
	}
	if !found {
		return ErrEngagementDenied
	}
	return nil
}

func sameEngagementSchema(left, right json.RawMessage) bool {
	var a, b any
	return decodeEngagementJSON(left, &a) == nil && decodeEngagementJSON(right, &b) == nil && equalEngagementJSON(a, b)
}

// Earlier numeric bindings shared a prefix with versions that rounded numbers
// through float64. Permit an old numeric digest only when its full schema has
// exact tokens that the rounded encoder could not have produced.
func unambiguousLegacyEngagementSchema(raw json.RawMessage, schema, fullSchema any) bool {
	if !engagementJSONHasNumber(schema) {
		return true
	}
	if fullSchema == nil {
		return false
	}
	var rounded any
	if json.Unmarshal(raw, &rounded) != nil {
		return true
	}
	exactJSON, _ := json.Marshal(fullSchema)
	roundedJSON, _ := json.Marshal(rounded)
	return !bytes.Equal(exactJSON, roundedJSON)
}

func engagementJSONHasNumber(value any) bool {
	switch value := value.(type) {
	case json.Number:
		return true
	case map[string]any:
		for _, child := range value {
			if engagementJSONHasNumber(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(value, engagementJSONHasNumber)
	}
	return false
}

type engagementCapabilityOption struct {
	ID        string
	Operation string
	Target    string
}

func (snap runtimeSnapshot) engagementCapabilities(actorID, dataChatID int64) map[string]EngagementCapability {
	result := make(map[string]EngagementCapability)
	if actorID > 0 && dataChatID == actorID {
		result["private:followup"] = privateFollowupCapability(actorID)
	}
	snap.visitEngagementCapabilities(actorID, dataChatID, func(id string, capability EngagementCapability, _ string) {
		result[id] = capability
	})
	return result
}

func (snap runtimeSnapshot) visitEngagementCapabilities(actorID, dataChatID int64, visit func(string, EngagementCapability, string)) {
	collect := func(registry *ToolRegistry, owner int64) {
		if registry == nil {
			return
		}
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		for name, tool := range registry.tools {
			remote, ok := tool.(*MCPRemoteTool)
			if !ok || remote.background == nil || remote.command != nil {
				continue
			}
			binding := remote.background
			write := binding.Operation == engagementCalendarCreate
			if write != (owner != 0) {
				continue
			}
			option := "public:"
			if write {
				option = "personal:"
			}
			visit(option+binding.server+":"+name, EngagementCapability{
				Access: MemoryDataAccess{OwnerID: owner, Key: binding.key}, Operation: binding.Operation,
				Target: binding.Target, DataChatID: dataChatID, Write: write,
			}, binding.legacyKey)
		}
	}
	collect(snap.tools, 0)
	if actorID > 0 && dataChatID == actorID && snap.userTools != nil {
		collect(snap.userTools.engagementNetworkRegistry(actorID), actorID)
	}
}

// Context lookup checks current discoveries once per relevant connection.
func (snap runtimeSnapshot) liveEngagementOptions(ctx context.Context, options map[string]EngagementCapability, initiator int64) []engagementCapabilityOption {
	type discovery struct {
		tools []mcp.Tool
		err   error
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	connections := make(map[*client.Client]discovery)
	available := make([]engagementCapabilityOption, 0, len(options))
	for id, opt := range options {
		if opt == privateFollowupCapability(initiator) {
			available = append(available, engagementCapabilityOption{id, opt.Operation, opt.Target})
			continue
		}
		resolved, err := snap.resolveEngagementCapability(opt, initiator)
		if err != nil {
			continue
		}
		c := resolved.tool.client
		result, listed := connections[c]
		if !listed {
			result.tools, result.err = listEngagementMCPTools(ctx, c)
			connections[c] = result
		}
		if result.err == nil && resolved.matchDiscovered(result.tools) == nil {
			available = append(available, engagementCapabilityOption{id, opt.Operation, opt.Target})
		}
	}
	slices.SortFunc(available, func(a, b engagementCapabilityOption) int { return strings.Compare(a.ID, b.ID) })
	return available
}

// resolveEngagementCapability checks the stored binding against the current
// runtime. Workers also verify the Telegram audience and check again at the
// operation boundary. A same-named connector or tool cannot match by name.
func (snap runtimeSnapshot) resolveEngagementCapability(capability EngagementCapability, initiatorID int64) (resolvedEngagementCapability, error) {
	if !capability.valid() ||
		(capability.Access.OwnerID != 0 && (capability.Access.OwnerID != initiatorID || capability.DataChatID != initiatorID)) {
		return resolvedEngagementCapability{}, ErrEngagementDenied
	}
	registry := snap.tools
	if capability.Access.OwnerID != 0 {
		if snap.userTools == nil {
			return resolvedEngagementCapability{}, ErrEngagementDenied
		}
		registry = snap.userTools.engagementNetworkRegistry(initiatorID)
	}
	if registry != nil {
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		for _, tool := range registry.tools {
			remote, ok := tool.(*MCPRemoteTool)
			if !ok || remote.background == nil || remote.command != nil {
				continue
			}
			binding := remote.background
			if (binding.key == capability.Access.Key || binding.legacyKey == capability.Access.Key) &&
				binding.Operation == capability.Operation &&
				binding.Target == capability.Target && capability.Write == (binding.Operation == engagementCalendarCreate) &&
				capability.Write == (capability.Access.OwnerID != 0) {
				return resolvedEngagementCapability{tool: remote}, nil
			}
		}
	}
	return resolvedEngagementCapability{}, ErrEngagementDenied
}

// verifyEngagementCapability admits an exact capability from an active
// agreement. The worker keeps its own claim and uses the verified connection
// and typed arguments only for the immediate operation.
func (b *Bot) verifyEngagementCapability(ctx context.Context, item Engagement, capability EngagementCapability) (resolvedEngagementCapability, error) {
	if item.Status != EngagementActive || item.Current == nil ||
		(capability.Write && !item.Current.Once) ||
		(capability != item.Current.Observe && !slices.Contains(item.Current.Actions, capability)) {
		return resolvedEngagementCapability{}, ErrEngagementDenied
	}
	if _, err := item.recipientSources(); err != nil {
		return resolvedEngagementCapability{}, errEngagementRoute
	}
	snap := b.snapshot()
	chat, err := snap.engagementChat(ctx, capability.DataChatID, item.Origin.SenderID)
	if err != nil {
		return resolvedEngagementCapability{}, err
	}
	resolved, err := snap.resolveEngagementCapability(capability, item.Origin.SenderID)
	if err != nil {
		return resolved, err
	}
	if err := resolved.verify(ctx); err != nil {
		return resolvedEngagementCapability{}, err
	}
	live := b.snapshot()
	if live.engagements == nil {
		return resolvedEngagementCapability{}, ErrEngagementDenied
	}
	effective, err := live.engagements.Get(item.Ref.ID)
	if err != nil {
		return resolvedEngagementCapability{}, err
	}
	if effective.Ref != item.Ref || effective.Status != EngagementActive ||
		(effective.Current != nil && !effective.Current.ExpiresAt.IsZero() && !live.engagements.now().Before(effective.Current.ExpiresAt)) {
		return resolvedEngagementCapability{}, ErrEngagementConflict
	}
	current, err := live.resolveEngagementCapability(capability, item.Origin.SenderID)
	if err != nil || current.tool != resolved.tool || !engagementChatAllowed(live.cfg, chat, item.Origin.SenderID) {
		return resolvedEngagementCapability{}, ErrEngagementDenied
	}
	return current, ctx.Err()
}
