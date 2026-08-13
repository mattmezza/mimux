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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrDisabled is returned when no API key is configured.
var ErrDisabled = errors.New("ai: disabled (no openrouter api key configured)")

// ErrAuth is returned when OpenRouter rejects the key (401/403) — permanent,
// and the only failure worth pointing the user at Settings for.
var ErrAuth = errors.New("ai: openrouter rejected the api key")

const apiURL = "https://openrouter.ai/api/v1/chat/completions"

// Retry tuning. OpenRouter (and the provider behind it) hands out 429s and
// short-lived 5xx/empty responses often enough that a second click usually
// works; these make chat do that click itself. chatBudget caps every attempt
// plus backoff, so retries stay inside the old single-shot timeout and the
// browser never waits longer than before.
// NOTE: fixed backoff, no jitter — knobs are maxAttempts, retryBackoff,
// maxRetryWait and chatBudget.
const (
	maxAttempts  = 3
	retryBackoff = 700 * time.Millisecond // multiplied by the attempt number
	maxRetryWait = 5 * time.Second        // ceiling on a Retry-After header
	chatBudget   = 60 * time.Second
)

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
// NOTE: characters, not tokens — no tokenizer dependency for a rough cap.
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

// chat is the single call path for every AI feature, so the retry lives here
// and all of them get it.
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
	ctx, cancel := context.WithTimeout(ctx, chatBudget)
	defer cancel()

	var lastErr error
	for attempt := 1; ; attempt++ {
		out, wait, err := c.chatOnce(ctx, body, attempt)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if wait == 0 || attempt == maxAttempts {
			return "", lastErr
		}
		slog.Warn("ai: transient failure, retrying", "attempt", attempt, "in", wait, "err", err)
		select {
		case <-ctx.Done():
			return "", lastErr
		case <-time.After(wait):
		}
	}
}

// chatOnce runs one request. On failure it returns how long to wait before the
// next attempt, or 0 when the failure is permanent (no key, bad key, bad
// request, unparseable reply) and retrying would only delay the honest error.
func (c *Client) chatOnce(ctx context.Context, reqBody []byte, attempt int) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil { // caller went away, or the budget is spent
			return "", 0, fmt.Errorf("ai: request failed: %w", err)
		}
		return "", backoff(attempt), fmt.Errorf("ai: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", backoff(attempt), fmt.Errorf("ai: read response: %w", err)
	}
	// Decoded before the status check but tolerated if it fails: a gateway 502
	// is an HTML page, and the status is the interesting part of it.
	var out chatResponse
	decodeErr := json.Unmarshal(data, &out)
	apiMsg := ""
	if out.Error != nil {
		apiMsg = out.Error.Message
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("ai: api returned status %d: %s", resp.StatusCode, detail(apiMsg, data))
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return "", 0, fmt.Errorf("%w (status %d): %s", ErrAuth, resp.StatusCode, detail(apiMsg, data))
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return "", retryAfter(resp.Header, attempt), err
		}
		return "", 0, err
	}
	if decodeErr != nil {
		return "", 0, fmt.Errorf("ai: decode response: %w (%s)", decodeErr, detail("", data))
	}
	if len(out.Choices) == 0 {
		// 200 with an error body (or with nothing in it) is a provider hiccup —
		// OpenRouter does this instead of a 5xx often enough to be worth a retry.
		return "", backoff(attempt), fmt.Errorf("ai: no completion returned: %s", detail(apiMsg, data))
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), 0, nil
}

func backoff(attempt int) time.Duration { return time.Duration(attempt) * retryBackoff }

// retryAfter honours OpenRouter's Retry-After (seconds), capped so one rate
// limit can't eat the whole budget, and falls back to the fixed backoff.
func retryAfter(h http.Header, attempt int) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		return min(time.Duration(s)*time.Second, maxRetryWait)
	}
	return backoff(attempt)
}

// detail prefers OpenRouter's error message and falls back to a truncated raw
// body, so a gateway's HTML error page still says something in the log.
func detail(apiMsg string, body []byte) string {
	if apiMsg != "" {
		return apiMsg
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
