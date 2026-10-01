// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/ai"
	"github.com/mattmezza/mimux/internal/store"
)

// The per-task reasoning level rides the same path as the per-task model:
// aiClient(feature) resolves it, so no call site has to know it exists.
func TestAIClientCarriesPerTaskReasoningEffort(t *testing.T) {
	s := serverWith(t, nil, nil)
	cfg := s.store.GetAppConfig()
	cfg.AIReasoningEffort = "low"
	cfg.AIThreadSummarizeReasoningEffort = "high"
	if err := s.store.SaveAppConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := s.aiClient(store.AISummarize).ReasoningEffort; got != "low" {
		t.Errorf("summarize reasoning = %q, want low (the global default)", got)
	}
	if got := s.aiClient(store.AIThreadSummarize).ReasoningEffort; got != "high" {
		t.Errorf("thread summarize reasoning = %q, want the override", got)
	}
}

// The integrations form saves each reasoning field, and an unknown value can
// never reach a request body — it falls through to the global default.
func TestSettingsReasoningEffortsSaveAndRender(t *testing.T) {
	s := serverWith(t, nil, nil)

	if rec := postSettings(t, settingsRouter(s), url.Values{
		"section":                     {"integrations"},
		"ai_reasoning":                {"medium"},
		"ai_thread_summary_reasoning": {"high"},
		"ai_refine_reasoning":         {"none"},
		"ai_compose_reasoning":        {"enormous"},
		"ai_options_reasoning":        {""},
		"ai_summarize_reasoning":      {"minimal"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("integrations save = %d: %s", rec.Code, rec.Body.String())
	}

	cfg := s.store.GetAppConfig()
	if cfg.AIComposeReasoningEffort != "" {
		t.Errorf("invalid value stored: %q", cfg.AIComposeReasoningEffort)
	}
	for f, want := range map[store.AIFeature]string{
		store.AISummarize:       "minimal", // its own override
		store.AICompose:         "medium",  // invalid override dropped, so inherits
		store.AIOptions:         "medium",  // blank override, so inherits
		store.AIThreadSummarize: "high",
		store.AIRefine:          "none",
	} {
		if got := cfg.ReasoningFor(f); got != want {
			t.Errorf("%s reasoning = %q, want %q", f, got, want)
		}
	}

	// One selector per task plus the global one, each with its check action,
	// and the full scale including Off.
	body := renderSection(t, s, "integrations")
	for _, want := range []string{
		`name="ai_reasoning"`, `name="ai_compose_reasoning"`, `name="ai_options_reasoning"`,
		`name="ai_refine_reasoning"`, `name="ai_summarize_reasoning"`,
		`name="ai_thread_summary_reasoning"`, `data-effort-check`, `value="none"`,
		`/ai/model-reasoning`,
		// The stored value is what the selector shows as selected.
		`value="medium" selected`, `value="high" selected`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("integrations page missing %q", want)
		}
	}
}

// GET /ai/model-reasoning serves the descriptor, and refuses an empty model
// rather than looking one up. A custom base URL has no OpenRouter catalogue,
// so it answers found=false — the UI keeps the full list, and no request ever
// leaves the process to discover that.
func TestModelReasoningEndpoint(t *testing.T) {
	r := chi.NewRouter()
	r.Mount("/ai", ai.Routes(func(store.AIFeature) *ai.Client {
		return &ai.Client{APIKey: "k", BaseURL: "http://127.0.0.1:1/v1"}
	}))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/model-reasoning?model=anthropic/claude-sonnet-4-6", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var info struct {
		Model             string `json:"model"`
		Found             bool   `json:"found"`
		SupportsReasoning bool   `json:"supports_reasoning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Found || info.SupportsReasoning || info.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("info = %+v", info)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/model-reasoning", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing model status = %d, want 400", rec.Code)
	}
}
