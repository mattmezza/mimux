// SPDX-License-Identifier: AGPL-3.0-only
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDraft_Disabled(t *testing.T) {
	c := &Client{Model: "test-model"}
	if _, err := c.Draft(context.Background(), "plain", "", "hello", false); err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

// A keyless client is only disabled when it has nowhere else to go: pointed at
// a local runner it must work, and must not send an empty bearer.
func TestDraft_KeylessLocalEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Errorf("sent an Authorization header without a key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"local reply"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "local", HTTPClient: srv.Client()}
	res, err := c.Draft(context.Background(), "plain", "", "hello", false)
	if err != nil || res.Draft != "local reply" {
		t.Fatalf("Draft = %+v, err = %v", res, err)
	}
}

func TestDraft_FreshCompose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-model" || len(req.Messages) != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		// system prompt carries the prefs + format
		if !strings.Contains(req.Messages[0].Content, "Markdown") ||
			!strings.Contains(req.Messages[0].Content, "friendly") ||
			!strings.Contains(req.Messages[0].Content, "concise") {
			t.Errorf("system prompt missing prefs/format: %q", req.Messages[0].Content)
		}
		if !strings.Contains(req.Messages[1].Content, "renew my domain") {
			t.Errorf("user prompt missing topic: %q", req.Messages[1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Subject: Domain renewal\n\nHi, please renew it."}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", HTTPClient: srv.Client(),
		Prefs: Prefs{Tone: "friendly", Brevity: "concise"}}
	res, err := c.Draft(context.Background(), "markdown", "", "renew my domain", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject != "Domain renewal" || res.Draft != "Hi, please renew it." {
		t.Errorf("Draft = %+v", res)
	}
}

func TestOptions_ParsesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(req.Messages[1].Content, "exactly 4") {
			t.Errorf("options prompt missing count: %q", req.Messages[1].Content)
		}
		// wrap the array in prose + code fence to test tolerant parsing
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"choices\":[{\"message\":{\"content\":\"Sure!\\n```json\\n[{\\\"label\\\":\\\"Accept\\\",\\\"gist\\\":\\\"Confirm attendance\\\"},{\\\"label\\\":\\\"Decline\\\",\\\"gist\\\":\\\"Politely say no\\\"}]\\n```\"}}]}"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTPClient: srv.Client(), Prefs: Prefs{ReplyOptions: 4}}
	opts, err := c.Options(context.Background(), "Are you coming?")
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 2 || opts[0].Label != "Accept" || opts[1].Gist != "Politely say no" {
		t.Errorf("Options = %+v", opts)
	}
}

func TestRefine_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "bad", Model: "m", HTTPClient: srv.Client()}
	if _, err := c.Refine(context.Background(), "plain", "hello", "shorter"); err == nil {
		t.Fatal("expected error")
	}
}

func TestChat_RetriesTransient(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "0") // ignored, falls back to the backoff
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTPClient: srv.Client()}
	got, err := c.Refine(context.Background(), "plain", "hello", "shorter")
	if err != nil || got != "ok" {
		t.Fatalf("Refine = %q, err = %v", got, err)
	}
	if hits != 2 {
		t.Errorf("attempts = %d, want 2", hits)
	}
}

func TestChat_NoRetryOnAuth(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "bad", Model: "m", HTTPClient: srv.Client()}
	_, err := c.Refine(context.Background(), "plain", "hello", "shorter")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if hits != 1 {
		t.Errorf("attempts = %d, want 1 (401 is permanent)", hits)
	}
	if msg := ErrMessage(err); !strings.Contains(msg, "API key") {
		t.Errorf("ErrMessage = %q", msg)
	}
}

func TestChat_CancelledContextSkipsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		cancel() // the browser walked away mid-flight
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTPClient: srv.Client()}
	start := time.Now()
	if _, err := c.Refine(ctx, "plain", "hello", "shorter"); err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > retryBackoff/2 {
		t.Errorf("slept out the backoff: %v", elapsed)
	}
	if hits != 1 {
		t.Errorf("attempts = %d, want 1", hits)
	}
}

func TestRefine_UnknownAction(t *testing.T) {
	c := &Client{APIKey: "k", Model: "m"}
	if _, err := c.Refine(context.Background(), "plain", "hi", "bogus"); err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestSummarize_LevelAndTruncation(t *testing.T) {
	long := strings.Repeat("a", maxSummaryChars+500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := req.Messages[1].Content
		if !strings.Contains(prompt, "single sentence of at most 25 words") {
			t.Errorf("prompt missing level instruction: %q", prompt)
		}
		if !strings.Contains(prompt, "cut off here") {
			t.Errorf("prompt missing truncation note: %q", prompt)
		}
		if !strings.Contains(prompt, "in Italian") {
			t.Errorf("prompt missing language: %q", prompt)
		}
		if n := strings.Count(prompt, "a"); n > maxSummaryChars+200 {
			t.Errorf("body not truncated: %d body chars in prompt", n)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  The invoice is due Friday.  "}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTPClient: srv.Client(), Prefs: Prefs{Language: "Italian"}}
	sum, truncated, err := c.Summarize(context.Background(), "oneline", long)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "The invoice is due Friday." || !truncated {
		t.Errorf("Summarize = %q, truncated=%v", sum, truncated)
	}
}

func TestSummarize_ShortBodyAndUnknownLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(req.Messages[1].Content, "bullet points") {
			t.Errorf("prompt missing brief instruction: %q", req.Messages[1].Content)
		}
		if strings.Contains(req.Messages[1].Content, "cut off here") {
			t.Errorf("short body should not be marked truncated: %q", req.Messages[1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"- one\n- two"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTPClient: srv.Client()}
	sum, truncated, err := c.Summarize(context.Background(), "brief", "Please pay the invoice.")
	if err != nil || truncated || sum != "- one\n- two" {
		t.Fatalf("Summarize = %q, truncated=%v, err=%v", sum, truncated, err)
	}
	if _, _, err := c.Summarize(context.Background(), "bogus", "hi"); err == nil {
		t.Fatal("expected error for unknown summary level")
	}
}

// TestSummarizeThread pins the one difference from Summarize: the prompt
// reads as a conversation, not a single email, while level/truncation/language
// handling stay the same.
func TestSummarizeThread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := req.Messages[1].Content
		if !strings.Contains(prompt, "email conversation") {
			t.Errorf("prompt does not read as a conversation: %q", prompt)
		}
		if !strings.Contains(prompt, "Summarize the whole conversation") {
			t.Errorf("prompt missing the whole-conversation instruction: %q", prompt)
		}
		if !strings.Contains(prompt, "compact structured summary") {
			t.Errorf("prompt missing level instruction: %q", prompt)
		}
		for _, want := range []string{"bob@example.com", `as "Me"`, "Key points", "Decisions", "Action items", "Status / next steps"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("thread prompt missing %q: %q", want, prompt)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"- alice asked about lunch\n- bob proposed Thursday"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTPClient: srv.Client()}
	ctx := BuildThreadContext(
		[]Msg{{From: "bob@example.com", Date: at(20), Text: "Thursday works for me."}},
		[]Msg{{From: "alice@example.com", Date: at(10), Text: "Lunch this week?"}},
	)
	sum, truncated, err := c.SummarizeThread(context.Background(), "brief", ctx, []string{"bob@example.com"})
	if err != nil || truncated {
		t.Fatalf("SummarizeThread = %q, truncated=%v, err=%v", sum, truncated, err)
	}
	if sum != "- alice asked about lunch\n- bob proposed Thursday" {
		t.Errorf("SummarizeThread = %q", sum)
	}
}

func TestClientURL(t *testing.T) {
	for base, want := range map[string]string{
		"":                             defaultAPIURL,
		"http://llama:8080/v1":         "http://llama:8080/v1/chat/completions",
		"http://llama:8080/v1/":        "http://llama:8080/v1/chat/completions",
		"http://x/v1/chat/completions": "http://x/v1/chat/completions",
	} {
		if got := (&Client{BaseURL: base}).url(); got != want {
			t.Errorf("url(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestSplitSubject(t *testing.T) {
	subj, body := splitSubject("Subject: Hello there\n\nThe body.")
	if subj != "Hello there" || body != "The body." {
		t.Errorf("got subj=%q body=%q", subj, body)
	}
	if s, b := splitSubject("No subject here."); s != "" || b != "No subject here." {
		t.Errorf("got subj=%q body=%q", s, b)
	}
}

func TestSystemPrompt_Language(t *testing.T) {
	auto := systemPrompt(Prefs{}.withDefaults(), "plain")
	if !strings.Contains(auto, "same language") || !strings.Contains(auto, "plain text") {
		t.Errorf("auto system prompt = %q", auto)
	}
	fixed := systemPrompt(Prefs{Language: "Italian"}.withDefaults(), "html")
	if !strings.Contains(fixed, "Italian") || !strings.Contains(fixed, "HTML") {
		t.Errorf("fixed system prompt = %q", fixed)
	}
}
