// SPDX-License-Identifier: AGPL-3.0-only
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// openRouterModelsURL is the provider catalogue the reasoning lookup reads. It
// is OpenRouter's list endpoint — the same provider defaultAPIURL talks to.
const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// modelCatalogueTTL is how long a fetched catalogue is reused. The catalogue is
// hundreds of entries and changes rarely, so one fetch serves every lookup; a
// lookup must never happen on page render or per keystroke.
const modelCatalogueTTL = 6 * time.Hour

// maxCatalogueBytes caps the catalogue read. The real payload is a few MB;
// anything far past that is not a catalogue and should not be buffered.
const maxCatalogueBytes = 16 << 20

// ReasoningInfo is the trimmed, UI-facing slice of one model's reasoning
// descriptor. Found is false when the model id is unknown or the catalogue
// could not be read; SupportsReasoning is false when the model exists but
// exposes no effort selection at all (non-reasoning models, dynamic routers
// such as openrouter/auto).
//
// SupportedEfforts nil means the model accepts every gateway value (OpenRouter
// documents a JSON null there), so the UI keeps its full list. Mandatory means
// the model rejects effort "none" and the off choice must be hidden.
type ReasoningInfo struct {
	Model             string   `json:"model"`
	Found             bool     `json:"found"`
	SupportsReasoning bool     `json:"supports_reasoning"`
	SupportedEfforts  []string `json:"supported_efforts"`
	DefaultEffort     string   `json:"default_effort,omitempty"`
	DefaultEnabled    bool     `json:"default_enabled"`
	Mandatory         bool     `json:"mandatory"`
}

// reasoningDescriptor is the raw per-model reasoning block from the catalogue.
// SupportedEfforts nil is meaningful: OpenRouter documents a JSON null there
// for a model that accepts every gateway value.
type reasoningDescriptor struct {
	SupportedEfforts []string `json:"supported_efforts"`
	DefaultEffort    string   `json:"default_effort"`
	DefaultEnabled   bool     `json:"default_enabled"`
	Mandatory        bool     `json:"mandatory"`
}

// modelEntry is one catalogue record. Reasoning is a pointer so an omitted
// descriptor (the model exposes no effort selection) is distinguishable from
// one present with a null supported_efforts (the model accepts every value).
type modelEntry struct {
	ID        string               `json:"id"`
	Reasoning *reasoningDescriptor `json:"reasoning"`
}

// The catalogue cache. One entry per process: the endpoint and key come from
// Settings, and a key/model change does not change the catalogue itself.
// NOTE: the mutex is held across the fetch, so concurrent lookups fetch once
// together rather than stampeding the provider.
var (
	catalogueMu sync.Mutex
	catalogue   []modelEntry
	catalogueAt time.Time
)

// ModelReasoning returns the reasoning descriptor for one model id.
//
// It reads the provider's model catalogue server-side, because the OpenRouter
// key must never reach the browser. A custom MIMUX_AI_BASE_URL points at some
// other OpenAI-compatible endpoint, which has no such catalogue — the lookup
// reports found=false and the settings UI keeps its full list working. Either
// way this is a convenience layered on the fixed effort set, never a
// dependency: callers must keep working when it fails.
func (c *Client) ModelReasoning(ctx context.Context, modelID string) (ReasoningInfo, error) {
	info := ReasoningInfo{Model: modelID}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return info, fmt.Errorf("ai: no model given")
	}
	if c.BaseURL != "" {
		// Not OpenRouter: no catalogue contract, so don't go fishing for one.
		return info, nil
	}
	entries, err := catalogued(ctx, c.HTTPClient, openRouterModelsURL, c.APIKey)
	if err != nil {
		return info, err
	}
	return reasoningFor(entries, modelID), nil
}

// reasoningFor picks one model out of a catalogue and trims its descriptor.
// An unknown id, and a model with no reasoning field at all, both come back
// Found/SupportsReasoning accordingly — the caller decides what to show.
func reasoningFor(entries []modelEntry, modelID string) ReasoningInfo {
	info := ReasoningInfo{Model: modelID}
	for _, e := range entries {
		if e.ID != modelID {
			continue
		}
		info.Found = true
		if e.Reasoning == nil {
			return info // model exists, exposes no effort selection
		}
		info.SupportsReasoning = true
		info.SupportedEfforts = e.Reasoning.SupportedEfforts
		info.DefaultEffort = e.Reasoning.DefaultEffort
		info.DefaultEnabled = e.Reasoning.DefaultEnabled
		info.Mandatory = e.Reasoning.Mandatory
		return info
	}
	return info // unknown id: found stays false
}

// catalogued returns the model catalogue, fetching and caching it on the first
// call (and again once the TTL has passed). A failed fetch is not cached, so
// the next lookup retries instead of pinning the failure for six hours.
func catalogued(ctx context.Context, hc *http.Client, url, apiKey string) ([]modelEntry, error) {
	catalogueMu.Lock()
	defer catalogueMu.Unlock()
	if catalogue != nil && time.Since(catalogueAt) < modelCatalogueTTL {
		return catalogue, nil
	}
	entries, err := fetchModelEntries(ctx, hc, url, apiKey)
	if err != nil {
		return nil, err
	}
	catalogue, catalogueAt = entries, time.Now()
	return entries, nil
}

// fetchModelEntries reads and decodes one model catalogue.
func fetchModelEntries(ctx context.Context, hc *http.Client, url, apiKey string) ([]modelEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai: fetch model catalogue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogueBytes))
	if err != nil {
		return nil, fmt.Errorf("ai: read model catalogue: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ai: model catalogue returned status %d", resp.StatusCode)
	}
	var payload struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("ai: decode model catalogue: %w", err)
	}
	return payload.Data, nil
}

// resetCatalogueCache drops the in-memory catalogue. Tests use it to keep one
// case's fetch from serving the next.
func resetCatalogueCache() {
	catalogueMu.Lock()
	defer catalogueMu.Unlock()
	catalogue, catalogueAt = nil, time.Time{}
}
