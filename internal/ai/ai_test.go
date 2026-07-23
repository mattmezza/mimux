package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDraft_Disabled(t *testing.T) {
	c := &Client{Model: "test-model"}
	if _, err := c.Draft(context.Background(), "plain", "", "hello", false); err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
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

	c := &Client{APIKey: "test-key", Model: "test-model", HTTPClient: srv.Client(),
		Prefs: Prefs{Tone: "friendly", Brevity: "concise"}}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
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

	c := &Client{APIKey: "k", Model: "m", HTTPClient: srv.Client(), Prefs: Prefs{ReplyOptions: 4}}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
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

	c := &Client{APIKey: "bad", Model: "m", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	if _, err := c.Refine(context.Background(), "plain", "hello", "shorter"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRefine_UnknownAction(t *testing.T) {
	c := &Client{APIKey: "k", Model: "m"}
	if _, err := c.Refine(context.Background(), "plain", "hi", "bogus"); err == nil {
		t.Fatal("expected error for unknown action")
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

// rewriteHostTransport redirects requests to the test server regardless of
// the URL's host, since Client.chat hardcodes the real API host.
type rewriteHostTransport struct{ base string }

func (rt rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base, err := http.NewRequest(req.Method, rt.base, nil)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = base.URL.Scheme
	req.URL.Host = base.URL.Host
	return http.DefaultTransport.RoundTrip(req)
}
