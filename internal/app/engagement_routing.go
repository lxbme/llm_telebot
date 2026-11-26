package app

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const chineseRouteDeliveryPattern = `(?:通知|[发發](?:送)?(?:消息)?[到给給])`

var (
	chineseRouteTarget   = regexp.MustCompile(chineseRouteDeliveryPattern + `\s*(?:我的私聊|本群)|私聊我|私訊我|私下通知我|我的私聊`)
	chineseRouteDelivery = regexp.MustCompile(chineseRouteDeliveryPattern)
	chineseRouteNegation = regexp.MustCompile(`(?:不[要用必得想能]|勿|[别別])\s*(?:再|直接)?\s*在?\s*$`)
	chineseRouteRequest  = regexp.MustCompile(`[请請]\s*你?\s*$`)
	chineseRouteBare     = regexp.MustCompile(`(?:^|[时時])\s*(?:就|但)?\s*$`)
	chineseRouteModifier = regexp.MustCompile(`\s*(?:(?:直接|只|[仅僅])\s*)+$`)
	chineseRouteJoin     = regexp.MustCompile(`^\s*(?:并且|並且|以及|[并並且但和或])\s*`)
	chineseRouteReported = regexp.MustCompile(`(?:[说說写寫](?:[着著道了])?|[叫让讓]|表示|提到|例如|比如|要求)\s*[:：]?\s*(?:[他她它](?:[们們])?\s*)?(?:[会會将將要]\s*)?$`)
)

// EngagementRecipientSource ties an approved destination to the authenticated
// message that established it. The model never supplies these records.
type EngagementRecipientSource struct {
	ChatID int64
	Source MemorySource
}

type engagementRouteAddress struct {
	id       int64
	relative engagementRouteRelative
}

type engagementRouteRelative uint8

const (
	engagementRouteCurrentChat engagementRouteRelative = iota + 1
	engagementRouteSender
)

func (a engagementRouteAddress) resolve(source MemorySource) int64 {
	switch a.relative {
	case engagementRouteCurrentChat:
		return source.ChatID
	case engagementRouteSender:
		return source.SenderID
	default:
		return a.id
	}
}

type engagementRoute struct {
	destinations []engagementRouteAddress
	excluded     []engagementRouteAddress
	ambiguous    bool
}

func (r *engagementRoute) add(address engagementRouteAddress, excluded bool) {
	if excluded {
		r.excluded = append(r.excluded, address)
	} else {
		r.destinations = append(r.destinations, address)
	}
}

// Bind numeric labels in a delivery position before considering a default.
// An incomplete address there requires clarification.
func parseEngagementRoute(message string) engagementRoute {
	var route engagementRoute
	if len(message) > 16*1024 {
		route.ambiguous = true
		return route
	}
	for _, sentence := range routeSentences(strings.ToLower(strings.ReplaceAll(message, "’", "'"))) {
		clause := make([]string, 0, 24)
		conditionStart := -1
		observing, delivering, excluded := false, false, false
		words := routeWords(sentence)
		for i := 0; i < len(words); i++ {
			word := words[i]
			switch word {
			case "and", "but":
				if i+1 < len(words) && (routeDeliveryVerb(words[i+1]) || words[i+1] == "do" || words[i+1] == "don't" || words[i+1] == "never") {
					clause = clause[:0]
					conditionStart, observing, delivering, excluded = -1, false, false, false
				}
			case ",", "，", "then":
				clause = clause[:0]
				conditionStart, observing, delivering, excluded = -1, false, false, false
			case "when", "whenever", "once", "if", "after", "before", "until":
				conditionStart, observing, delivering, excluded = len(clause)+1, false, false, false
			case "watch", "monitor", "observe", "check", "track", "follow":
				observing = !delivering
			case "not", "never", "don't", "except", "excluding":
				excluded = true
			}
			if routeDeliveryVerb(word) {
				if conditionStart >= 0 && !routeSourceSubject(clause[conditionStart:]) {
					// The directive followed a condition without a comma.
					conditionStart = -1
				}
				delivering, observing = true, false
			}

			previous := ""
			if len(clause) > 0 {
				previous = clause[len(clause)-1]
			}
			clause = append(clause, word)
			if conditionStart >= 0 || observing {
				continue
			}
			if delivering {
				if id, consumed, found := routeLabeledAddress(clause, words[i+1:]); found {
					if id == 0 {
						route.ambiguous = true
					} else {
						route.add(engagementRouteAddress{id: id}, excluded)
						clause = append(clause, words[i+1:i+1+consumed]...)
						i += consumed
					}
					continue
				}
				tail := strings.Join(clause[max(0, len(clause)-3):], " ")
				switch {
				case word == "privately", tail == "my private chat", tail == "to my dm",
					previous == "dm" && word == "me", previous == "in" && word == "private":
					route.add(engagementRouteAddress{relative: engagementRouteSender}, excluded)
				case word == "here",
					(word == "chat" || word == "group") && (previous == "this" || previous == "current") &&
						routeDestinationLead(clause[:len(clause)-2]):
					route.add(engagementRouteAddress{relative: engagementRouteCurrentChat}, excluded)
				}
			}
			negative := strings.HasPrefix(word, "-") && routeStartsNumber(word)
			if !negative {
				continue
			}
			switch previous {
			case "version", "release", "build", "date":
				continue
			}
			id, err := strconv.ParseInt(word, 10, 64)
			if err != nil || id == 0 {
				route.ambiguous = true
				continue
			}
			// The Chinese branch classifies explicit numeric chat references.
			prefix := strings.Join(clause[:len(clause)-1], " ")
			directives := chineseRouteDelivery.FindAllStringIndex(prefix, -1)
			chineseDelivery := len(directives) > 0
			var chineseExclusion bool
			if chineseDelivery {
				_, chineseExclusion = splitChineseRouteNegation(prefix[:directives[len(directives)-1][0]])
			}
			chineseSource := strings.Contains(prefix, "观察") || strings.Contains(prefix, "觀察") || strings.Contains(prefix, "关注")
			switch {
			case chineseSource && !chineseDelivery:
			case delivering || chineseDelivery:
				route.add(engagementRouteAddress{id: id}, excluded || chineseExclusion)
			default:
				route.ambiguous = true
			}
		}
	}
	route.addChineseDirectives(dialogueQuoted.ReplaceAllString(message, " "))
	return route
}

func (r *engagementRoute) addChineseDirectives(message string) {
	for _, phrase := range strings.FieldsFunc(message, func(c rune) bool {
		return strings.ContainsRune(".,，。!！?？;；\n", c)
	}) {
		phrase = strings.Join(strings.Fields(phrase), " ")
		lastDirectiveEnd := 0
		for _, match := range chineseRouteTarget.FindAllStringIndex(phrase, -1) {
			address := phrase[match[0]:match[1]]
			lead := phrase[:match[0]]
			// A joined predicate can inherit the preceding directive's
			// imperative form while keeping its own route and negation.
			if lastDirectiveEnd > 0 {
				tail := phrase[lastDirectiveEnd:match[0]]
				if join := chineseRouteJoin.FindStringIndex(tail); join != nil {
					lead = tail[join[1]:]
				}
			}
			prefix, excluded := splitChineseRouteNegation(chineseRouteModifier.ReplaceAllString(lead, ""))
			// 请 can follow a condition or introduce someone else's reported
			// request. Inspect its lead before treating it as a directive.
			if request := chineseRouteRequest.FindStringIndex(prefix); request != nil {
				var negatedRequest bool
				prefix, negatedRequest = splitChineseRouteNegation(prefix[:request[0]])
				excluded = excluded || negatedRequest
			} else if !excluded && !chineseRouteBare.MatchString(prefix) {
				continue
			}
			if chineseReportedRequest(prefix) || address == "我的私聊" && !excluded {
				continue
			}
			// A possessive or plural extends the object past "我" or "本群".
			// It requires clarification instead of granting a partial match.
			suffix := strings.TrimSpace(phrase[match[1]:])
			if strings.HasPrefix(suffix, "的") || strings.HasPrefix(suffix, "们") || strings.HasPrefix(suffix, "們") {
				r.ambiguous = true
				continue
			}
			target := engagementRouteSender
			if strings.HasSuffix(address, "本群") {
				target = engagementRouteCurrentChat
			}
			r.add(engagementRouteAddress{relative: target}, excluded)
			lastDirectiveEnd = match[1]
		}
	}
}

func chineseReportedRequest(prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	// 重写了 is the completion of a condition, whereas 写了 can
	// introduce a reported request.
	return chineseRouteReported.MatchString(prefix) &&
		!strings.HasSuffix(prefix, "重写了") && !strings.HasSuffix(prefix, "重寫了")
}

func splitChineseRouteNegation(prefix string) (string, bool) {
	match := chineseRouteNegation.FindStringIndex(prefix)
	if match == nil {
		return prefix, false
	}
	// In 特别 and 特別 the last character is part of the adjective.
	negation := prefix[match[0]:]
	if strings.HasSuffix(prefix[:match[0]], "特") &&
		(strings.HasPrefix(negation, "别") || strings.HasPrefix(negation, "別")) {
		return prefix, false
	}
	return prefix[:match[0]], true
}

// A found label with a zero ID is an unresolved address.
func routeLabeledAddress(clause, tail []string) (id int64, consumed int, found bool) {
	label := clause[len(clause)-1]
	lead := clause[:len(clause)-1]
	telegram := len(lead) > 0 && lead[len(lead)-1] == "telegram"
	user := label == "user" || label == "id" && telegram
	if !user && label != "chat" && label != "group" {
		return 0, 0, false
	}
	if telegram {
		lead = lead[:len(lead)-1]
	}
	if !routeDestinationLead(lead) {
		return 0, 0, false
	}
	explicit := telegram
	if len(tail) > 0 && tail[0] == "id" {
		consumed++
		explicit = true
	}
	for consumed < len(tail) && routeIDSeparator(tail[consumed]) {
		consumed++
		explicit = true
	}
	if consumed == len(tail) {
		return 0, 0, explicit
	}
	token := tail[consumed]
	id, err := strconv.ParseInt(token, 10, 64)
	if err != nil || id == 0 || user != (id > 0) {
		return 0, 0, explicit || routeStartsNumber(token)
	}
	if routeContinuesAddress(tail[consumed+1:]) {
		return 0, 0, true
	}
	return id, consumed + 1, true
}

func routeContinuesAddress(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "/":
		return true
	case ",", "，", "and", "or":
		if (words[0] == "," || words[0] == "，") && routeStartsDateClause(words[1:]) {
			return false
		}
		words = words[1:]
		for len(words) > 0 && (words[0] == "to" || words[0] == "the") {
			words = words[1:]
		}
		if len(words) == 0 {
			return false
		}
		switch words[0] {
		case "telegram":
			return len(words) > 1 && (words[1] == "user" || words[1] == "id")
		case "user", "group", "chat":
			return len(words) > 1 && (words[1] == "id" || routeIDSeparator(words[1]) || routeStartsNumber(words[1]))
		default:
			number := strings.TrimPrefix(words[0], "-")
			return number != "" && strings.Trim(number, "0123456789") == ""
		}
	}
	return false
}

// A slash date starting a new statement is separate from the destination.
// An adjacent slash, or bare numbers after a comma, can still name recipients.
func routeStartsDateClause(words []string) bool {
	if len(words) < 4 || words[1] != "/" || (words[3] != "is" && words[3] != "was") {
		return false
	}
	_, err := time.Parse("1/2", strings.Join(words[:3], ""))
	return err == nil
}

func routeStartsNumber(word string) bool {
	word = strings.TrimPrefix(word, "-")
	return len(word) > 0 && word[0] >= '0' && word[0] <= '9'
}

func routeIDSeparator(word string) bool {
	switch word {
	case ":", "：", "=", "＝", "#", "＃", "-", "/":
		return true
	}
	return false
}

func routeDestinationLead(clause []string) bool {
	for len(clause) > 0 {
		word := clause[len(clause)-1]
		switch word {
		case "only", "the", ":", "：":
			clause = clause[:len(clause)-1]
		case "to", "in", "except", "excluding":
			return true
		default:
			return routeDeliveryVerb(word)
		}
	}
	return false
}

func routeDeliveryVerb(word string) bool {
	switch word {
	case "tell", "notify", "alert", "message", "ping", "remind", "send", "post", "deliver", "report", "dm", "reply":
		return true
	}
	return false
}

// A delivery verb in a conditional clause denotes a source only when
// the clause supplies a third-party subject. A bare directive such as
// "when it ships send the update" starts a new destination clause.
func routeSourceSubject(words []string) bool {
	if len(words) == 2 && words[0] == "the" {
		words = words[1:]
	}
	if len(words) != 1 {
		return false
	}
	switch words[0] {
	case "maintainers", "maintainer", "admins", "admin", "team", "upstream", "they", "someone", "publisher", "service", "calendar":
		return true
	}
	return false
}

// A period inside a numeric token belongs to the token. This keeps a
// release version inside its clause and refuses decimals as Telegram IDs.
func routeSentences(message string) []string {
	var sentences []string
	start := 0
	for i, c := range message {
		if !strings.ContainsRune(".。!！?？;；\n", c) {
			continue
		}
		if c == '.' && i > 0 && i+1 < len(message) &&
			message[i-1] >= '0' && message[i-1] <= '9' && message[i+1] >= '0' && message[i+1] <= '9' {
			continue
		}
		sentences = append(sentences, message[start:i])
		start = i + len(string(c))
	}
	return append(sentences, message[start:])
}

func routeWords(sentence string) []string {
	var words []string
	start := -1
	for i, c := range sentence {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '\'' || c == '.' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			words = append(words, sentence[start:i])
			start = -1
		}
		if strings.ContainsRune(",，:：=＝#＃/", c) {
			words = append(words, string(c))
		}
	}
	if start >= 0 {
		words = append(words, sentence[start:])
	}
	return words
}

func (r engagementRoute) permits(source MemorySource, recipients []int64) bool {
	if r.ambiguous {
		return false
	}
	for _, address := range r.excluded {
		if slices.Contains(recipients, address.resolve(source)) {
			return false
		}
	}
	for _, address := range r.destinations {
		if !slices.Contains(recipients, address.resolve(source)) {
			return false
		}
	}
	return true
}

func (r engagementRoute) actualRecipient(source MemorySource) string {
	if r.ambiguous || len(r.destinations) == 0 {
		return ""
	}
	id := r.destinations[0].resolve(source)
	for _, address := range r.destinations[1:] {
		if address.resolve(source) != id {
			return ""
		}
	}
	if id == source.ChatID {
		return "here"
	}
	if id == source.SenderID {
		return "private"
	}
	return ""
}

func (r engagementRoute) names(source MemorySource, id int64) bool {
	return slices.ContainsFunc(r.destinations, func(address engagementRouteAddress) bool {
		return address.resolve(source) == id
	})
}

// Older rows have accepted decisions but no dedicated routing record. Replay
// the acceptance chain, including accepted revisions that left the numeric
// destination unchanged. A mismatched chain needs a fresh explicit decision.
func (e Engagement) recipientSources() ([]EngagementRecipientSource, error) {
	if e.Current == nil || len(e.Current.Recipients) == 0 {
		return nil, ErrEngagementDenied
	}
	if grants := e.Current.RecipientSources; len(grants) != 0 {
		if len(grants) != len(e.Current.Recipients) {
			return nil, ErrEngagementDenied
		}
		for i, grant := range grants {
			if grant.ChatID != e.Current.Recipients[i] || !e.recipientSourceMatched(grant) {
				return nil, ErrEngagementDenied
			}
		}
		return grants, nil
	}
	var grants []EngagementRecipientSource
	for _, decision := range e.Decisions {
		if decision.Verdict != EngagementAccept || (decision.Change != EngagementAgree && decision.Change != EngagementReaccept) {
			continue
		}
		source, a := decision.Request.Source, decision.Effective
		route := parseEngagementRoute(decision.Request.Text)
		if a == nil || !source.valid() || strings.TrimSpace(decision.Request.Text) == "" || !route.permits(source, a.Recipients) {
			return nil, ErrEngagementDenied
		}
		next := make([]EngagementRecipientSource, 0, len(a.Recipients))
		for _, id := range a.Recipients {
			i := slices.IndexFunc(grants, func(g EngagementRecipientSource) bool { return g.ChatID == id })
			switch {
			case i >= 0:
				next = append(next, grants[i])
			case id == source.ChatID && (id < 0 || source.SenderID == id && e.Origin.SenderID == id):
				next = append(next, EngagementRecipientSource{id, source})
			case id == source.SenderID && id == e.Origin.SenderID && route.names(source, id):
				next = append(next, EngagementRecipientSource{id, source})
			default:
				return nil, ErrEngagementDenied
			}
		}
		grants = next
	}
	if !slices.EqualFunc(e.Current.Recipients, grants, func(id int64, g EngagementRecipientSource) bool { return id == g.ChatID }) {
		return nil, ErrEngagementDenied
	}
	return grants, nil
}

func (e Engagement) recipientSourceMatched(grant EngagementRecipientSource) bool {
	if grant.ChatID == 0 || !grant.Source.valid() ||
		grant.ChatID > 0 && (grant.ChatID != e.Origin.SenderID || grant.Source.SenderID != grant.ChatID) ||
		grant.ChatID < 0 && grant.Source.ChatID != grant.ChatID {
		return false
	}
	matched := false
	for _, decision := range e.Decisions {
		if decision.Verdict != EngagementAccept || (decision.Change != EngagementAgree && decision.Change != EngagementReaccept) ||
			decision.Effective == nil || !slices.Contains(decision.Effective.Recipients, grant.ChatID) {
			continue
		}
		source := decision.Request.Source
		if !matched && source != grant.Source {
			continue
		}
		route := parseEngagementRoute(decision.Request.Text)
		if !source.valid() || !route.permits(source, decision.Effective.Recipients) {
			return false
		}
		if !matched {
			matched = grant.ChatID == source.ChatID || route.names(source, grant.ChatID)
		}
	}
	return matched
}

func (e Engagement) authorizeRecipientSources(d EngagementDecision) error {
	grants := d.Authorized.RecipientSources
	if len(grants) == 0 {
		return nil // older application callers; runtime still checks their decision chain
	}
	recipients, source := d.Agreement.Recipients, d.Request.Source
	route := parseEngagementRoute(d.Request.Text)
	if !route.permits(source, recipients) || len(grants) != len(recipients) {
		return ErrEngagementDenied
	}
	previous, _ := e.recipientSources()
	for i, grant := range grants {
		if grant.ChatID != recipients[i] {
			return ErrEngagementDenied
		}
		if slices.Contains(previous, grant) {
			continue
		}
		if grant.Source != source || grant.ChatID != source.ChatID && !route.names(source, grant.ChatID) ||
			grant.ChatID > 0 && (grant.ChatID != source.SenderID || grant.ChatID != e.Origin.SenderID) ||
			grant.ChatID < 0 && grant.ChatID != source.ChatID {
			return ErrEngagementDenied
		}
	}
	return nil
}

func (d *engagementDialogue) bindRecipients(item Engagement, decision EngagementDecision, recipients []int64) ([]EngagementRecipientSource, error) {
	source := decision.Request.Source
	route := parseEngagementRoute(decision.Request.Text)
	if !route.permits(source, recipients) {
		return nil, ErrEngagementDenied
	}
	previous, previousErr := item.recipientSources()
	grants := make([]EngagementRecipientSource, 0, len(recipients))
	for _, id := range recipients {
		prior := slices.IndexFunc(previous, func(g EngagementRecipientSource) bool { return g.ChatID == id })
		switch {
		case id == source.ChatID && (item.Current == nil || route.names(source, id)):
			grants = append(grants, EngagementRecipientSource{id, source})
		case id == source.SenderID && route.names(source, id) && (item.Current == nil || item.Origin.SenderID == id):
			grants = append(grants, EngagementRecipientSource{id, source})
		case previousErr == nil && prior >= 0:
			grants = append(grants, previous[prior])
		default:
			return nil, ErrEngagementDenied
		}
	}
	return grants, nil
}
