package extraction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const claudeAPIURL = "https://api.anthropic.com/v1/messages"

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Usage is the token cost of one API call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

func callClaude(ctx context.Context, apiKey, model, prompt string) (string, error) {
	text, _, err := callClaudeUsage(ctx, apiKey, model, prompt)
	return text, err
}

// callClaudeUsage is callClaude plus the token cost, so callers that run in a
// loop can measure and report what they spend.
func callClaudeUsage(ctx context.Context, apiKey, model, prompt string) (string, Usage, error) {
	reqBody := claudeRequest{
		Model:     model,
		MaxTokens: 2048,
		Messages: []claudeMessage{
			{Role: "user", Content: prompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == 429 {
		wait := 60 * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				wait = time.Duration(secs) * time.Second
			}
		}
		return "", Usage{}, fmt.Errorf("rate limited, retry after %v", wait)
	}

	// Model not found — signal caller to fall back
	if resp.StatusCode == 404 && strings.Contains(string(respBody), "not_found_error") {
		return "", Usage{}, &ModelNotFoundError{Model: model}
	}

	if resp.StatusCode != 200 {
		return "", Usage{}, fmt.Errorf("api error %d: %s", resp.StatusCode, string(respBody))
	}

	var result claudeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", Usage{}, fmt.Errorf("parse response: %w", err)
	}

	if result.Error != nil {
		return "", Usage{}, fmt.Errorf("claude error: %s", result.Error.Message)
	}

	if len(result.Content) == 0 {
		return "", Usage{}, fmt.Errorf("empty response from claude")
	}

	return result.Content[0].Text, Usage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens}, nil
}

// ModelNotFoundError signals that the requested model doesn't exist for this API key.
type ModelNotFoundError struct {
	Model string
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("model not found: %s", e.Model)
}
