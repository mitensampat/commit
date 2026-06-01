package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/msfoundry/commit/store"
)

func generateNudgeMessage(ctx context.Context, apiKey, model string, c *store.Commitment) (string, error) {
	prompt := fmt.Sprintf(`Write a short, natural WhatsApp follow-up message (1-2 sentences max) for this situation:

- %s promised to: %s
- Context: %s
- Original quote: "%s"
- This was %s ago

The message should be polite, casual, and natural — like something a real person would type on WhatsApp. Don't be formal or robotic. Don't use greetings like "Hi" or "Hey there". Just a friendly nudge about the thing.

Return ONLY the message text, nothing else.`, c.PersonName, c.Title, c.Context, c.SourceQuote, c.SourceTime)

	text, err := callClaudeSimple(ctx, apiKey, model, prompt, 256)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func callClaudeSimple(ctx context.Context, apiKey, model, prompt string, maxTokens int) (string, error) {
	reqBody := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode == 404 && strings.Contains(string(respBody), "not_found_error") {
		return "", fmt.Errorf("model_not_found:%s", model)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("api error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response")
	}

	return result.Content[0].Text, nil
}

// callNudgeWithFallback tries the preferred model, falls back if not found
func (s *Server) callNudgeWithFallback(ctx context.Context, apiKey string, c *store.Commitment) (string, error) {
	model := s.db.GetModel()
	text, err := generateNudgeMessage(ctx, apiKey, model, c)
	if err != nil && strings.Contains(err.Error(), "model_not_found") && model != store.FallbackModel {
		log.Printf("model %s not available for nudge, falling back to %s", model, store.FallbackModel)
		s.db.SetModel(store.FallbackModel)
		return generateNudgeMessage(ctx, apiKey, store.FallbackModel, c)
	}
	return text, err
}
