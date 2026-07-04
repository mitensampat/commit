package extraction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/msfoundry/commit/store"
)

type FindResult struct {
	Answer  string `json:"answer"`
	Sources []struct {
		Type    string `json:"type"`
		Chat    string `json:"chat"`
		Person  string `json:"person"`
		Date    string `json:"date"`
		Content string `json:"content"`
	} `json:"sources"`
}

func (e *Extractor) FindAnswer(ctx context.Context, query string) (string, error) {
	result, err := e.Find(ctx, query)
	if err != nil {
		return "", err
	}
	return result.Answer, nil
}

func (e *Extractor) Find(ctx context.Context, query string) (*FindResult, error) {
	apiKey := e.db.GetAPIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("API key not configured")
	}

	keywords := extractKeywords(query)
	if len(keywords) == 0 {
		return nil, fmt.Errorf("could not extract search terms from query")
	}

	expanded := expandKeywords(keywords)

	// Step 1: Identify person names — try resolving each keyword as a person
	var personJIDs []string
	var topicKeywords []string
	for _, kw := range keywords {
		jids := e.db.ResolvePerson(kw)
		if len(jids) > 0 {
			personJIDs = append(personJIDs, jids...)
		} else {
			topicKeywords = append(topicKeywords, kw)
		}
	}

	// Step 2: Search messages with person-aware strategy
	seen := map[string]bool{}
	var msgs []*store.Message

	expandedTopics := expandKeywords(topicKeywords)
	if len(expandedTopics) == 0 {
		expandedTopics = expanded
	}

	// Tier 1: If we have a person AND topic, search within their chats first
	if len(personJIDs) > 0 && len(topicKeywords) > 0 {
		personMsgs, _ := e.db.SearchMessagesInChats(personJIDs, expandedTopics, 50)
		for _, m := range personMsgs {
			if !seen[m.ID] {
				seen[m.ID] = true
				msgs = append(msgs, m)
			}
		}
	}

	// Tier 2: AND search across all chats
	andMsgs, _ := e.db.SearchMessagesAND(expanded, 50)
	for _, m := range andMsgs {
		if !seen[m.ID] {
			seen[m.ID] = true
			msgs = append(msgs, m)
		}
	}

	// Tier 3: OR search — broadest net
	if len(msgs) < 20 {
		orMsgs, _ := e.db.SearchMessages(expanded, 100)
		for _, m := range orMsgs {
			if !seen[m.ID] {
				seen[m.ID] = true
				msgs = append(msgs, m)
			}
			if len(msgs) >= 100 {
				break
			}
		}
	}

	// Step 3: Conversation threading — for top hits, pull surrounding context
	var threaded []*store.Message
	threadSeen := map[string]bool{}
	threadCount := 0
	for _, m := range msgs {
		if threadCount >= 8 {
			break
		}
		surrounding, err := e.db.GetMessagesAround(m.ChatJID, m.Timestamp.Unix(), 5)
		if err != nil || len(surrounding) == 0 {
			if !threadSeen[m.ID] {
				threadSeen[m.ID] = true
				threaded = append(threaded, m)
			}
			threadCount++
			continue
		}
		for _, sm := range surrounding {
			if !threadSeen[sm.ID] {
				threadSeen[sm.ID] = true
				threaded = append(threaded, sm)
			}
		}
		threadCount++
	}

	// Step 4: Search commitments
	commitments, _ := e.db.SearchCommitments(strings.Join(expanded, " "))

	if len(threaded) == 0 && len(commitments) == 0 {
		return &FindResult{
			Answer: "I couldn't find any messages or commitments matching your query. Try different keywords or person names.",
		}, nil
	}

	// Score and rank commitments by keyword overlap
	rankedCommitments := rankCommitments(commitments, expanded)

	prompt := buildFindPrompt(query, threaded, rankedCommitments)
	model := e.db.GetModel()
	response, err := callClaude(ctx, apiKey, model, prompt)
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}

	jsonStr := extractJSON(response)
	var result FindResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return &FindResult{Answer: response}, nil
	}

	return &result, nil
}

func rankCommitments(commitments []*store.Commitment, keywords []string) []*store.Commitment {
	type scored struct {
		c     *store.Commitment
		score int
	}
	var items []scored
	for _, c := range commitments {
		s := 0
		combined := strings.ToLower(c.Title + " " + c.PersonName + " " + c.ChatName + " " + c.Context)
		for _, kw := range keywords {
			if strings.Contains(combined, kw) {
				s++
			}
		}
		items = append(items, scored{c, s})
	}
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if items[j].score > items[i].score {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	var out []*store.Commitment
	for _, sc := range items {
		out = append(out, sc.c)
	}
	return out
}

// expandKeywords adds stemming variants (plural/singular) to catch more matches.
func expandKeywords(keywords []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kw := range keywords {
		if seen[kw] {
			continue
		}
		seen[kw] = true
		out = append(out, kw)
		// Add singular if plural (strip trailing 's')
		if len(kw) > 3 && strings.HasSuffix(kw, "s") {
			singular := kw[:len(kw)-1]
			if !seen[singular] {
				seen[singular] = true
				out = append(out, singular)
			}
		}
		// Add plural if singular
		plural := kw + "s"
		if !seen[plural] {
			seen[plural] = true
			out = append(out, plural)
		}
	}
	return out
}

func buildFindPrompt(query string, msgs []*store.Message, commitments []*store.Commitment) string {
	var sb strings.Builder
	sb.WriteString(`You are a sharp executive assistant with perfect recall. The user is asking about something from their WhatsApp conversations. Answer their question precisely using the context below.

The messages are grouped as conversation threads — surrounding messages are included for context so you can understand the flow.

Rules:
- Be specific: names, dates, exact quotes when relevant
- If you find the exact conversation, quote the key messages
- If you find related but not exact matches, say so clearly
- If the context doesn't contain enough to answer, say what you did find and suggest what to search for
- Keep answers concise — 2-4 sentences unless the user needs detail
- Use the user's timezone (IST)

Return JSON: {"answer": "your answer here", "sources": [{"type": "message" or "commitment", "chat": "chat name", "person": "person name", "date": "readable date", "content": "relevant snippet"}]}

User's question: `)
	sb.WriteString(query)
	sb.WriteString("\n\n")

	if len(msgs) > 0 {
		// Group messages by chat for readability
		type chatThread struct {
			name string
			msgs []*store.Message
		}
		chatOrder := []string{}
		chatMap := map[string]*chatThread{}
		for _, m := range msgs {
			key := m.ChatJID
			if _, ok := chatMap[key]; !ok {
				name := m.ChatName
				if name == "" {
					name = m.SenderName
				}
				chatMap[key] = &chatThread{name: name}
				chatOrder = append(chatOrder, key)
			}
			chatMap[key].msgs = append(chatMap[key].msgs, m)
		}

		sb.WriteString("=== CONVERSATION THREADS ===\n")
		for _, key := range chatOrder {
			t := chatMap[key]
			sb.WriteString(fmt.Sprintf("\n--- Chat: %s ---\n", t.name))
			for _, m := range t.msgs {
				prefix := m.SenderName
				if m.IsFromMe {
					prefix = "[ME]"
				}
				sb.WriteString(fmt.Sprintf("[%s] %s: %s\n",
					m.Timestamp.Format("Jan 2, 3:04 PM"),
					prefix,
					m.Content,
				))
			}
		}
		sb.WriteString("\n")
	}

	if len(commitments) > 0 {
		sb.WriteString("=== MATCHING COMMITMENTS ===\n")
		limit := 20
		if len(commitments) < limit {
			limit = len(commitments)
		}
		for _, c := range commitments[:limit] {
			dir := "You owe"
			if c.Direction == "they_owe" {
				dir = "They owe"
			}
			sb.WriteString(fmt.Sprintf("[%s] %s: %s — %s (%s) [%s]\n",
				c.CreatedAt.Format("Jan 2"),
				dir, c.Title, c.PersonName, c.ChatName, c.Status,
			))
			if c.SourceQuote != "" {
				sb.WriteString(fmt.Sprintf("  Quote: \"%s\"\n", c.SourceQuote))
			}
		}
	}

	return sb.String()
}

var stopWords = map[string]bool{
	"i": true, "me": true, "my": true, "we": true, "our": true, "you": true, "your": true,
	"he": true, "she": true, "it": true, "they": true, "them": true, "their": true,
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true, "have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true, "will": true, "would": true, "could": true,
	"should": true, "may": true, "might": true, "shall": true, "can": true,
	"to": true, "of": true, "in": true, "for": true, "on": true, "with": true,
	"at": true, "by": true, "from": true, "about": true, "into": true, "through": true,
	"that": true, "this": true, "these": true, "those": true, "what": true, "which": true,
	"who": true, "whom": true, "when": true, "where": true, "how": true, "why": true,
	"and": true, "or": true, "but": true, "not": true, "no": true, "if": true,
	"so": true, "than": true, "too": true, "very": true, "just": true,
	"said": true, "tell": true, "told": true, "say": true, "saying": true,
	"find": true, "hey": true, "something": true, "thing": true, "things": true,
	"didn't": true, "don't": true, "there": true, "here": true,
	"also": true, "some": true, "any": true, "all": true, "each": true, "every": true,
}

func extractKeywords(query string) []string {
	words := strings.Fields(strings.ToLower(query))
	var keywords []string
	seen := map[string]bool{}

	for _, w := range words {
		w = strings.Trim(w, ".,!?\"'()[]{}@#")
		if len(w) < 2 || stopWords[w] {
			continue
		}
		if !seen[w] {
			keywords = append(keywords, w)
			seen[w] = true
		}
	}

	if len(keywords) > 8 {
		keywords = keywords[:8]
	}
	return keywords
}
