// Package ai drafts email compose/reply text via an OpenRouter chat
// completion model.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrDisabled is returned when no API key is configured.
var ErrDisabled = errors.New("ai: disabled (no openrouter api key configured)")

const apiURL = "https://openrouter.ai/api/v1/chat/completions"

// Client talks to the OpenRouter chat completions API.
type Client struct {
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewClient builds a Client with a sane default timeout.
func NewClient(apiKey, model string) *Client {
	return &Client{
		APIKey:     apiKey,
		Model:      model,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Compose drafts a new email about topic.
func (c *Client) Compose(ctx context.Context, topic string) (string, error) {
	prompt, err := composePrompt(topic)
	if err != nil {
		return "", err
	}
	return c.chat(ctx, prompt)
}

// Reply drafts a reply given the thread context and optional instructions.
func (c *Client) Reply(ctx context.Context, threadContext, instructions string) (string, error) {
	prompt, err := replyPrompt(threadContext, instructions)
	if err != nil {
		return "", err
	}
	return c.chat(ctx, prompt)
}

func (c *Client) chat(ctx context.Context, userPrompt string) (string, error) {
	if c == nil || c.APIKey == "" {
		return "", ErrDisabled
	}
	body, err := json.Marshal(chatRequest{
		Model: c.Model,
		Messages: []message{
			{Role: "system", Content: systemPrompt()},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ai: read response: %w", err)
	}
	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("ai: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != nil && out.Error.Message != "" {
			return "", fmt.Errorf("ai: api error: %s", out.Error.Message)
		}
		return "", fmt.Errorf("ai: api returned status %d", resp.StatusCode)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("ai: no completion returned")
	}
	return out.Choices[0].Message.Content, nil
}
