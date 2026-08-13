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
	"strings"
	"time"
)

// ErrDisabled is returned when no API key is configured.
var ErrDisabled = errors.New("ai: disabled (no openrouter api key configured)")

const apiURL = "https://openrouter.ai/api/v1/chat/completions"

// Prefs are the user-tunable knobs (Settings → AI) folded into every prompt.
type Prefs struct {
	Tone         string // professional|neutral|friendly|casual
	Brevity      string // concise|normal|detailed
	ReplyOptions int    // number of reply directions to generate (2-5)
	Language     string // "auto" (match the message) or a fixed language name
}

func (p Prefs) withDefaults() Prefs {
	if p.Tone == "" {
		p.Tone = "neutral"
	}
	if p.Brevity == "" {
		p.Brevity = "normal"
	}
	if p.ReplyOptions < 2 || p.ReplyOptions > 5 {
		p.ReplyOptions = 3
	}
	if p.Language == "" {
		p.Language = "auto"
	}
	return p
}

// Option is one suggested reply direction shown as a chip.
type Option struct {
	Label string `json:"label"`
	Gist  string `json:"gist"`
}

// DraftResult is a generated draft plus an optional suggested subject (only
// filled when a subject was requested).
type DraftResult struct {
	Draft   string
	Subject string
}

// Client talks to the OpenRouter chat completions API.
type Client struct {
	APIKey     string
	Model      string
	Prefs      Prefs
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

// Options asks the model for N candidate reply directions given the original
// message. N comes from Prefs (2-5).
func (c *Client) Options(ctx context.Context, threadContext string) ([]Option, error) {
	p := c.Prefs.withDefaults()
	prompt, err := optionsPrompt(threadContext, p.ReplyOptions)
	if err != nil {
		return nil, err
	}
	raw, err := c.chat(ctx, "You suggest email reply directions. Respond with JSON only.", prompt)
	if err != nil {
		return nil, err
	}
	return parseOptions(raw)
}

// Draft generates a full draft. An empty threadContext means a fresh compose,
// in which case direction is the topic; otherwise direction is the chosen reply
// direction/gist. format is the compose mode (plain|html|markdown). When
// wantSubject is set the model is asked to prepend a "Subject:" line, which is
// parsed out into DraftResult.Subject.
func (c *Client) Draft(ctx context.Context, format, threadContext, direction string, wantSubject bool) (DraftResult, error) {
	prompt, err := draftPrompt(threadContext, direction, wantSubject)
	if err != nil {
		return DraftResult{}, err
	}
	raw, err := c.chat(ctx, systemPrompt(c.Prefs.withDefaults(), format), prompt)
	if err != nil {
		return DraftResult{}, err
	}
	subject, body := splitSubject(raw)
	return DraftResult{Draft: body, Subject: subject}, nil
}

var refineActions = map[string]string{
	"shorter":    "shorter and more to the point",
	"formal":     "more formal and professional",
	"friendlier": "warmer and friendlier",
}

// Refine rewrites text per action (shorter|formal|friendlier), keeping the
// output in the given format.
func (c *Client) Refine(ctx context.Context, format, text, action string) (string, error) {
	instr, ok := refineActions[action]
	if !ok {
		return "", fmt.Errorf("ai: unknown refine action %q", action)
	}
	prompt, err := refinePrompt(text, instr)
	if err != nil {
		return "", err
	}
	return c.chat(ctx, systemPrompt(c.Prefs.withDefaults(), format), prompt)
}

var summaryLevels = map[string]string{
	"oneline":  "a single sentence of at most 25 words",
	"brief":    `3 to 5 short bullet points, one line each, every line starting with "- "`,
	"detailed": `one short paragraph of context, then "- " bullets covering every key fact, date, number and action item`,
}

// maxSummaryChars is the body budget sent to the model — long enough for any
// real email, short enough to keep one summary cheap. Longer bodies are cut and
// the caller is told so it can say as much in the UI.
// ponytail: characters, not tokens — no tokenizer dependency for a rough cap.
const maxSummaryChars = 12000

// Summarize condenses a message's plain-text body at the given detail level
// (oneline|brief|detailed). truncated reports whether the body was cut to
// maxSummaryChars before being sent.
func (c *Client) Summarize(ctx context.Context, level, body string) (summary string, truncated bool, err error) {
	instr, ok := summaryLevels[level]
	if !ok {
		return "", false, fmt.Errorf("ai: unknown summary level %q", level)
	}
	if r := []rune(body); len(r) > maxSummaryChars {
		body, truncated = strings.TrimSpace(string(r[:maxSummaryChars])), true
	}
	prompt, err := summarizePrompt(body, instr, c.Prefs.withDefaults().Language, truncated)
	if err != nil {
		return "", truncated, err
	}
	out, err := c.chat(ctx, "You summarize emails for a busy reader. Output only the summary, as plain text.", prompt)
	return out, truncated, err
}

// parseOptions extracts a JSON array of options from a model reply, tolerating
// code fences or stray prose around it.
func parseOptions(s string) ([]Option, error) {
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("ai: options response is not a JSON array: %q", s)
	}
	var opts []Option
	if err := json.Unmarshal([]byte(s[i:j+1]), &opts); err != nil {
		return nil, fmt.Errorf("ai: parse options: %w", err)
	}
	if len(opts) == 0 {
		return nil, errors.New("ai: no reply options returned")
	}
	return opts, nil
}

// splitSubject peels a leading "Subject: ..." line off a draft, if present.
func splitSubject(s string) (subject, body string) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, "Subject:")
	if !ok {
		return "", s
	}
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		return strings.TrimSpace(rest[:nl]), strings.TrimSpace(rest[nl+1:])
	}
	return strings.TrimSpace(rest), ""
}

func (c *Client) chat(ctx context.Context, sysPrompt, userPrompt string) (string, error) {
	if c == nil || c.APIKey == "" {
		return "", ErrDisabled
	}
	body, err := json.Marshal(chatRequest{
		Model: c.Model,
		Messages: []message{
			{Role: "system", Content: sysPrompt},
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
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
