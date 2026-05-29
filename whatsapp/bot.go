package whatsapp

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types/events"
)

func (c *Client) handleBotCommand(ctx context.Context, evt *events.Message) bool {
	if !evt.Info.IsFromMe {
		return false
	}
	if !c.isSelfChat(evt) {
		return false
	}

	text := extractText(evt.Message)
	if text == "" {
		return false
	}

	cmd := strings.TrimSpace(strings.ToLower(text))
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
	default:
		return false
	}

	if response != "" {
		_ = c.SendMessage(ctx, evt.Info.Chat, response)
	}
	return true
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
	var matches []*commitmentRef
	var displayName string
	for _, cm := range openCommitments {
		if strings.Contains(strings.ToLower(cm.PersonName), person) ||
			strings.Contains(strings.ToLower(cm.ChatName), person) {
			matches = append(matches, &commitmentRef{cm.Title, cm.Direction, cm.PersonName})
			if displayName == "" {
				if strings.Contains(strings.ToLower(cm.ChatName), person) {
					displayName = cm.ChatName
				} else {
					displayName = cm.PersonName
				}
			}
		}
	}

	if len(matches) == 0 {
		resolvedCommitments, _ := c.db.GetCommitments("resolved")
		resolvedCount := 0
		for _, cm := range resolvedCommitments {
			if strings.Contains(strings.ToLower(cm.PersonName), person) ||
				strings.Contains(strings.ToLower(cm.ChatName), person) {
				resolvedCount++
			}
		}
		if resolvedCount > 0 {
			return fmt.Sprintf("No open commitments with \"%s\" (%d resolved).", person, resolvedCount)
		}
		return fmt.Sprintf("No commitments found with \"%s\".", person)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Commitments with %s:*\n\n", displayName))
	for _, m := range matches {
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

commitments — list all open commitments
owe @person — what you owe someone
done <text> — mark a commitment resolved
search <query> — find commitments
help — show this message`
}

type commitmentRef struct {
	Title     string
	Direction string
	PersonName string
}
