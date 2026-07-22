// Package translate calls the Google Translate v2 REST API and caches
// results in SQLite.
package translate

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
var ErrDisabled = errors.New("translate: disabled (no api key configured)")

const apiURL = "https://translation.googleapis.com/language/translate/v2"

// Client talks to the Google Translate v2 API.
type Client struct {
	APIKey     string
	Target     string
	HTTPClient *http.Client
}

// NewClient builds a Client with a sane default timeout.
func NewClient(apiKey, target string) *Client {
	return &Client{
		APIKey:     apiKey,
		Target:     target,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

type requestBody struct {
	Q      string `json:"q"`
	Target string `json:"target"`
	Format string `json:"format"`
}

type apiResponse struct {
	Data struct {
		Translations []struct {
			TranslatedText     string `json:"translatedText"`
			DetectedSourceLang string `json:"detectedSourceLanguage"`
		} `json:"translations"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Translate returns the translated text and the detected source language.
func (c *Client) Translate(ctx context.Context, text string) (translated, detectedLang string, err error) {
	if c == nil || c.APIKey == "" {
		return "", "", ErrDisabled
	}
	body, err := json.Marshal(requestBody{Q: text, Target: c.Target, Format: "text"})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"?key="+c.APIKey, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("translate: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("translate: read response: %w", err)
	}
	var out apiResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", fmt.Errorf("translate: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Error != nil && out.Error.Message != "" {
			return "", "", fmt.Errorf("translate: api error: %s", out.Error.Message)
		}
		return "", "", fmt.Errorf("translate: api returned status %d", resp.StatusCode)
	}
	if len(out.Data.Translations) == 0 {
		return "", "", errors.New("translate: no translation returned")
	}
	t := out.Data.Translations[0]
	return t.TranslatedText, t.DetectedSourceLang, nil
}
