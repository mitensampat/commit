package whatsapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/msfoundry/commit/store"
	"go.mau.fi/whatsmeow/types/events"
)

func (c *Client) handleBotCommand(ctx context.Context, evt *events.Message) bool {
	if !evt.Info.IsFromMe {
		return false
	}

	text := extractText(evt.Message)
	if text == "" {
		return false
	}

	lower := strings.TrimSpace(strings.ToLower(text))

	// @find — context-aware search, works in self-chat
	if strings.HasPrefix(lower, "@find") {
		query := strings.TrimSpace(text[5:]) // preserve original case
		if query == "" {
			_ = c.SendMessage(ctx, evt.Info.Chat, "Usage: @find <your question>\n\nExamples:\n@find what did I tell Aakrit about meeting next week?\n@find when did Steve say he'd finish the project?")
			return true
		}
		go func() {
			_ = c.SendMessage(ctx, evt.Info.Chat, "🔍 Searching...")
			answer, err := c.findHandler.FindAnswer(ctx, query)
			if err != nil {
				_ = c.SendMessage(ctx, evt.Info.Chat, fmt.Sprintf("Search error: %v", err))
				return
			}
			_ = c.SendMessage(ctx, evt.Info.Chat, answer)
		}()
		return true
	}

	// @commit context pull — works in any chat
	if strings.HasPrefix(lower, "@commit") {
		query := strings.TrimSpace(strings.TrimPrefix(lower, "@commit"))
		response := c.cmdContextPull(evt, query)
		if response != "" {
			_ = c.SendMessage(ctx, evt.Info.Chat, response)
		}
		return true
	}

	// Self-chat commands only
	if !c.isSelfChat(evt) {
		return false
	}

	cmd := lower
	var response string

	switch {
	case cmd == "commitments" || cmd == "c":
		response = c.cmdListCommitments()
	case strings.HasPrefix(cmd, "owe "):
		person := strings.TrimPrefix(cmd, "owe ")
		person = strings.TrimPrefix(person, "@")
		response = c.cmdOwe(person)
	case strings.HasPrefix(cmd, "done "):
		query := strings.TrimPrefix(cmd, "done ")
		response = c.cmdDone(query)
	case strings.HasPrefix(cmd, "search "):
		query := strings.TrimPrefix(cmd, "search ")
		response = c.cmdSearch(query)
	case cmd == "help" || cmd == "h":
		response = c.cmdHelp()
	case len(cmd) == 1 && cmd[0] >= 'a' && cmd[0] <= 'z':
		response = c.cmdDisambiguate(cmd)
	default:
		return false
	}

	if response != "" {
		_ = c.SendMessage(ctx, evt.Info.Chat, response)
	}
	return true
}

func (c *Client) cmdContextPull(evt *events.Message, query string) string {
	chatJID := evt.Info.Chat.String()
	commitments, err := c.db.GetOpenCommitmentsForChat(chatJID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	if query != "" {
		query = strings.ToLower(query)
		var filtered []*store.Commitment
		for _, cm := range commitments {
			if strings.Contains(strings.ToLower(cm.Title), query) ||
				strings.Contains(strings.ToLower(cm.Context), query) {
				filtered = append(filtered, cm)
			}
		}
		commitments = filtered
	}

	if len(commitments) == 0 {
		if query != "" {
			return fmt.Sprintf("No open commitments matching \"%s\" in this chat.", query)
		}
		return "No open commitments in this chat."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*%d open commitments in this chat:*\n\n", len(commitments)))
	for i, cm := range commitments {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("\n...and %d more", len(commitments)-10))
			break
		}
		arrow := "→ You owe:"
		if cm.Direction == "they_owe" {
			arrow = "← They owe:"
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", arrow, cm.Title))
		if cm.DueHint != "" {
			sb.WriteString(fmt.Sprintf("  📅 %s\n", cm.DueHint))
		}
	}
	return sb.String()
}

func (c *Client) cmdListCommitments() string {
	commitments, err := c.db.GetCommitments("open")
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if len(commitments) == 0 {
		return "No open commitments."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*%d open commitments:*\n\n", len(commitments)))

	youOwe := 0
	theyOwe := 0
	for _, cm := range commitments {
		if cm.Direction == "you_owe" {
			youOwe++
		} else {
			theyOwe++
		}
	}
	sb.WriteString(fmt.Sprintf("You owe: %d | They owe: %d\n\n", youOwe, theyOwe))

	for i, cm := range commitments {
		if i >= 15 {
			sb.WriteString(fmt.Sprintf("\n...and %d more", len(commitments)-15))
			break
		}
		arrow := "→"
		if cm.Direction == "they_owe" {
			arrow = "←"
		}
		sb.WriteString(fmt.Sprintf("%s %s (%s)\n", arrow, cm.Title, cm.PersonName))
	}
	return sb.String()
}

func (c *Client) cmdOwe(person string) string {
	openCommitments, err := c.db.GetCommitments("open")
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	person = strings.ToLower(person)

	// Group matches by distinct person
	type personMatch struct {
		displayName string
		items       []*commitmentRef
	}
	seen := map[string]*personMatch{}
	var order []string

	for _, cm := range openCommitments {
		if fuzzyMatch(person, cm.PersonName) || fuzzyMatch(person, cm.ChatName) {
			key := strings.ToLower(cm.PersonName)
			if key == "" {
				key = strings.ToLower(cm.ChatName)
			}
			if _, ok := seen[key]; !ok {
				seen[key] = &personMatch{displayName: cm.PersonName}
				if seen[key].displayName == "" {
					seen[key].displayName = cm.ChatName
				}
				order = append(order, key)
			}
			seen[key].items = append(seen[key].items, &commitmentRef{cm.Title, cm.Direction, cm.PersonName})
		}
	}

	if len(seen) == 0 {
		resolvedCommitments, _ := c.db.GetCommitments("resolved")
		resolvedCount := 0
		for _, cm := range resolvedCommitments {
			if fuzzyMatch(person, cm.PersonName) || fuzzyMatch(person, cm.ChatName) {
				resolvedCount++
			}
		}
		if resolvedCount > 0 {
			return fmt.Sprintf("No open commitments with \"%s\" (%d resolved).", person, resolvedCount)
		}
		return fmt.Sprintf("No commitments found with \"%s\".", person)
	}

	// Single person match — show directly
	if len(seen) == 1 {
		pm := seen[order[0]]
		return formatPersonCommitments(pm.displayName, pm.items)
	}

	// Multiple people match — ask to disambiguate
	c.pendingMu.Lock()
	c.pendingChoices = make([]string, len(order))
	for i, key := range order {
		c.pendingChoices[i] = seen[key].displayName
	}
	c.pendingMu.Unlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("I found %d matches for \"%s\". Did you mean:\n\n", len(order), person))
	for i, key := range order {
		pm := seen[key]
		letter := string(rune('a' + i))
		sb.WriteString(fmt.Sprintf("(%s) %s (%d commitments)\n", letter, pm.displayName, len(pm.items)))
	}
	sb.WriteString("\nReply with a letter to choose.")
	return sb.String()
}

func (c *Client) cmdDisambiguate(letter string) string {
	c.pendingMu.Lock()
	choices := c.pendingChoices
	c.pendingChoices = nil
	c.pendingMu.Unlock()

	if len(choices) == 0 {
		return ""
	}

	idx := int(letter[0] - 'a')
	if idx < 0 || idx >= len(choices) {
		return fmt.Sprintf("Invalid choice. Please pick a-" + string(rune('a'+len(choices)-1)) + ".")
	}

	person := choices[idx]
	openCommitments, err := c.db.GetCommitments("open")
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	personLower := strings.ToLower(person)
	var items []*commitmentRef
	for _, cm := range openCommitments {
		if strings.ToLower(cm.PersonName) == personLower || strings.ToLower(cm.ChatName) == personLower {
			items = append(items, &commitmentRef{cm.Title, cm.Direction, cm.PersonName})
		}
	}

	if len(items) == 0 {
		return fmt.Sprintf("No open commitments with %s.", person)
	}

	return formatPersonCommitments(person, items)
}

func formatPersonCommitments(name string, items []*commitmentRef) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Commitments with %s:*\n\n", name))
	for _, m := range items {
		arrow := "→ You owe:"
		if m.Direction == "they_owe" {
			arrow = "← They owe:"
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", arrow, m.Title))
	}
	return sb.String()
}

func (c *Client) cmdDone(query string) string {
	commitments, err := c.db.GetCommitments("open")
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	query = strings.ToLower(query)
	for _, cm := range commitments {
		if strings.Contains(strings.ToLower(cm.Title), query) {
			if err := c.db.UpdateCommitmentStatus(cm.ID, "resolved"); err != nil {
				return fmt.Sprintf("Error resolving: %v", err)
			}
			return fmt.Sprintf("Resolved: %s", cm.Title)
		}
	}
	return fmt.Sprintf("No matching open commitment for \"%s\".", query)
}

func (c *Client) cmdSearch(query string) string {
	results, err := c.db.SearchCommitments(query)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if len(results) == 0 {
		return fmt.Sprintf("No results for \"%s\".", query)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*%d results for \"%s\":*\n\n", len(results), query))
	for i, cm := range results {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("\n...and %d more", len(results)-10))
			break
		}
		arrow := "→"
		if cm.Direction == "they_owe" {
			arrow = "←"
		}
		sb.WriteString(fmt.Sprintf("%s %s (%s)\n", arrow, cm.Title, cm.PersonName))
	}
	return sb.String()
}

func (c *Client) cmdHelp() string {
	return `*Commit Bot Commands:*

@find <question> — ask your EA anything about your chats
commitments — list all open commitments
owe @person — what you owe someone
done <text> — mark a commitment resolved
search <query> — find commitments
help — show this message

*In any chat:*
@commit — show open commitments for that chat
@commit <query> — search commitments in that chat`
}

type commitmentRef struct {
	Title      string
	Direction  string
	PersonName string
}

// fuzzyMatch checks if all words in the query appear as prefixes of words in the target.
// "rah" matches "Rahul Sharma", "rah sh" matches "Rahul Sharma", "sharma" matches "Rahul Sharma".
func fuzzyMatch(query, target string) bool {
	if target == "" {
		return false
	}
	target = strings.ToLower(target)
	// First try simple substring (covers exact and partial matches)
	if strings.Contains(target, query) {
		return true
	}
	// Then try all-words-match: each query word must prefix-match some target word
	queryWords := strings.Fields(query)
	targetWords := strings.Fields(target)
	for _, qw := range queryWords {
		found := false
		for _, tw := range targetWords {
			if strings.HasPrefix(tw, qw) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
