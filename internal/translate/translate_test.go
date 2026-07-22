package translate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mattmezza/sm/internal/store"
)

func TestTranslate_Disabled(t *testing.T) {
	c := &Client{Target: "en"}
	if _, _, err := c.Translate(context.Background(), "ciao"); err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

func TestTranslate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("missing api key in query")
		}
		var body requestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Q != "ciao" || body.Target != "en" {
			t.Errorf("unexpected request body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"translations":[{"translatedText":"hello","detectedSourceLanguage":"it"}]}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "test-key", Target: "en", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	translated, lang, err := c.Translate(context.Background(), "ciao")
	if err != nil {
		t.Fatal(err)
	}
	if translated != "hello" || lang != "it" {
		t.Errorf("Translate = %q, %q", translated, lang)
	}
}

func TestTranslate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid target"}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "test-key", Target: "xx", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	if _, _, err := c.Translate(context.Background(), "ciao"); err == nil {
		t.Fatal("expected error")
	}
}

func TestTranslationCache(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	key := store.TranslationCacheKey("ciao", "en")
	if _, _, ok, err := s.TranslationCached(key); err != nil || ok {
		t.Fatalf("expected cache miss, got ok=%v err=%v", ok, err)
	}
	if err := s.SaveTranslation(key, "hello", "it"); err != nil {
		t.Fatal(err)
	}
	translated, lang, ok, err := s.TranslationCached(key)
	if err != nil || !ok {
		t.Fatalf("expected cache hit, got ok=%v err=%v", ok, err)
	}
	if translated != "hello" || lang != "it" {
		t.Errorf("cached = %q, %q", translated, lang)
	}
}

// rewriteHostTransport redirects requests to the test server regardless of
// the URL's host, since Client.Translate hardcodes the real API host.
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
