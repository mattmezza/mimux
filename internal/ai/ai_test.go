package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompose_Disabled(t *testing.T) {
	c := &Client{Model: "test-model"}
	if _, err := c.Compose(context.Background(), "hello"); err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

func TestCompose_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-model" || len(req.Messages) != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		if !strings.Contains(req.Messages[1].Content, "vacation request") {
			t.Errorf("prompt missing topic: %q", req.Messages[1].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Dear team, ..."}}]}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "test-key", Model: "test-model", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	draft, err := c.Compose(context.Background(), "vacation request")
	if err != nil {
		t.Fatal(err)
	}
	if draft != "Dear team, ..." {
		t.Errorf("Compose = %q", draft)
	}
}

func TestReply_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "bad-key", Model: "test-model", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	if _, err := c.Reply(context.Background(), "thread", "be brief"); err == nil {
		t.Fatal("expected error")
	}
}

func TestPromptTemplates(t *testing.T) {
	if !strings.Contains(systemPrompt(), "plain-text") {
		t.Errorf("system prompt missing expected content: %q", systemPrompt())
	}
	got, err := composePrompt("renew my domain")
	if err != nil || !strings.Contains(got, "renew my domain") {
		t.Errorf("composePrompt = %q, err=%v", got, err)
	}
	got, err = replyPrompt("Alice: hi\nBob: hello", "keep it short")
	if err != nil || !strings.Contains(got, "Alice: hi") || !strings.Contains(got, "keep it short") {
		t.Errorf("replyPrompt = %q, err=%v", got, err)
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
