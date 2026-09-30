// SPDX-License-Identifier: AGPL-3.0-only
package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func entry(id string, reasoning *reasoningDescriptor) modelEntry {
	return modelEntry{ID: id, Reasoning: reasoning}
}

func reasoningEntry(efforts []string, def string, enabled, mandatory bool) *reasoningDescriptor {
	return &reasoningDescriptor{efforts, def, enabled, mandatory}
}

// The whole lookup path: read the catalogue, find the model, trim its
// descriptor, and make sure the response carries every field the UI reads.
func TestModelReasoning_FromCatalogue(t *testing.T) {
	resetCatalogueCache()
	t.Cleanup(resetCatalogueCache)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"plain/model"},
			{"id":"google/gemini-3.5-flash","reasoning":{"supported_efforts":["high","medium","low","minimal"],"default_effort":"medium","default_enabled":true,"mandatory":true}}
		]}`))
	}))
	defer srv.Close()

	old := openRouterModelsURL
	openRouterModelsURL = srv.URL
	t.Cleanup(func() { openRouterModelsURL = old })

	c := &Client{APIKey: "k", Model: "google/gemini-3.5-flash", HTTPClient: srv.Client()}
	got, err := c.ModelReasoning(context.Background(), "  google/gemini-3.5-flash  ")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || !got.SupportsReasoning || !got.Mandatory || !got.DefaultEnabled ||
		got.DefaultEffort != "medium" || len(got.SupportedEfforts) != 4 {
		t.Fatalf("got %+v", got)
	}
	if got.Model != "google/gemini-3.5-flash" {
		t.Errorf("model = %q, want it trimmed", got.Model)
	}

	// The endpoint's contract: every field present, so the UI never has to
	// guess at a missing one.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"model", "found", "supports_reasoning",
		"supported_efforts", "default_effort", "default_enabled", "mandatory"} {
		if !strings.Contains(string(b), `"`+field+`"`) {
			t.Errorf("response missing %q: %s", field, b)
		}
	}

	// A model that exposes no reasoning block at all: found, not capable.
	got, err = c.ModelReasoning(context.Background(), "plain/model")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.SupportsReasoning {
		t.Errorf("plain = %+v", got)
	}
}

func TestReasoningFor(t *testing.T) {
	entries := []modelEntry{
		entry("plain/model", nil),
		entry("gemini/flash", reasoningEntry([]string{"high", "medium", "low", "minimal"}, "medium", true, true)),
		// JSON null supported_efforts: the model accepts every gateway value.
		entry("open/router", reasoningEntry(nil, "", false, false)),
	}

	got := reasoningFor(entries, "gemini/flash")
	if !got.Found || !got.SupportsReasoning || got.Mandatory != true ||
		got.DefaultEffort != "medium" || !got.DefaultEnabled ||
		len(got.SupportedEfforts) != 4 || got.SupportedEfforts[0] != "high" {
		t.Errorf("gemini = %+v", got)
	}

	// Null supported_efforts stays nil so the UI keeps its full list, and the
	// model is still reported as reasoning-capable.
	got = reasoningFor(entries, "open/router")
	if !got.Found || !got.SupportsReasoning || got.SupportedEfforts != nil || got.Mandatory {
		t.Errorf("router = %+v", got)
	}

	// A model that exists but exposes no descriptor: found, not capable.
	got = reasoningFor(entries, "plain/model")
	if !got.Found || got.SupportsReasoning {
		t.Errorf("plain = %+v", got)
	}

	// Unknown id: the UI keeps the full list and says so.
	got = reasoningFor(entries, "nope/model")
	if got.Found || got.SupportsReasoning {
		t.Errorf("unknown = %+v", got)
	}
	if got.Model != "nope/model" {
		t.Errorf("model echoed back as %q", got.Model)
	}
}

// A custom base URL is not OpenRouter: there is no catalogue contract to read,
// so the lookup answers found=false without any network call. The setting must
// keep working in that configuration.
func TestModelReasoning_CustomBaseURL(t *testing.T) {
	c := &Client{APIKey: "k", BaseURL: "http://127.0.0.1:1/v1"}
	got, err := c.ModelReasoning(context.Background(), "any/model")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Found || got.SupportsReasoning || got.Model != "any/model" {
		t.Fatalf("got %+v", got)
	}
}

func TestModelReasoning_EmptyModel(t *testing.T) {
	c := &Client{}
	if _, err := c.ModelReasoning(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for a blank model")
	}
}

// One fetch serves every lookup: the catalogue is hundreds of entries and the
// TTL is the only thing that expires it.
func TestCatalogueFetchIsCached(t *testing.T) {
	resetCatalogueCache()
	t.Cleanup(resetCatalogueCache)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"a/b","reasoning":{"supported_efforts":["high"],"default_effort":"high","default_enabled":true}}]}`))
	}))
	defer srv.Close()

	for i := 0; i < 3; i++ {
		entries, err := catalogued(context.Background(), srv.Client(), srv.URL, "test-key")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].ID != "a/b" || entries[0].Reasoning == nil {
			t.Fatalf("entries = %+v", entries)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("catalogue fetched %d times, want 1", n)
	}
}

// A failed fetch is not cached, so the next lookup retries instead of pinning
// the failure for the whole TTL.
func TestCatalogueFailureIsNotCached(t *testing.T) {
	resetCatalogueCache()
	t.Cleanup(resetCatalogueCache)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hits.Load() == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"a/b"}]}`))
	}))
	defer srv.Close()

	if _, err := catalogued(context.Background(), srv.Client(), srv.URL, ""); err == nil {
		t.Fatal("expected the first fetch to fail")
	}
	entries, err := catalogued(context.Background(), srv.Client(), srv.URL, "")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if len(entries) != 1 || hits.Load() != 2 {
		t.Errorf("entries = %+v, hits = %d", entries, hits.Load())
	}
}

func TestCatalogueRejectsNonJSON(t *testing.T) {
	resetCatalogueCache()
	t.Cleanup(resetCatalogueCache)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not a catalogue</html>"))
	}))
	defer srv.Close()

	if _, err := catalogued(context.Background(), srv.Client(), srv.URL, ""); err == nil {
		t.Fatal("expected a decode error")
	}
}
