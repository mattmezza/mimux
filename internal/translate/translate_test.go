package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mattmezza/mimux/internal/store"
)

func TestTranslateHTML_Disabled(t *testing.T) {
	c := &Client{Target: "en"}
	if _, _, err := c.TranslateHTML(context.Background(), "<p>ciao</p>"); err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

func TestTranslateHTML_PreservesStructure(t *testing.T) {
	api := &stubAPI{}
	c, srv := stubClient(t, api)
	defer srv.Close()

	const doc = `<!doctype html><html><head><style>p { color: #f00 }</style></head>` +
		`<body><table><tr><td bgcolor="#eee"><p>Ciao <b>mondo</b></p>` +
		`<img src="data:image/gif;base64,AAA" alt="Buongiorno">` +
		`<script>var x = "non tradurre";</script></td></tr></table>` +
		`<pre>  riga uno  </pre></body></html>`

	out, lang, err := c.TranslateHTML(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if lang != "it" {
		t.Errorf("detected lang = %q, want it", lang)
	}

	// Only the prose was sent — no whitespace-only nodes, no script/style bodies.
	want := []string{"Ciao", "mondo", "riga uno"}
	if got := api.sent(); !slices.Equal(got, want) {
		t.Errorf("segments sent = %q, want %q", got, want)
	}

	// Markup, attributes and skipped content come back untouched.
	for _, keep := range []string{
		`<style>p { color: #f00 }</style>`,
		`<script>var x = "non tradurre";</script>`,
		`<td bgcolor="#eee">`,
		`<img src="data:image/gif;base64,AAA" alt="Buongiorno"/>`,
		`<b>`, `</table>`,
		`data-mimux-lang="it"`,
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("output lost %q:\n%s", keep, out)
		}
	}
	// Text is translated in place, and the whitespace the API strips is restored.
	if !strings.Contains(out, `<p>T:Ciao <b>T:mondo</b></p>`) {
		t.Errorf("text not translated in place:\n%s", out)
	}
	if !strings.Contains(out, `<pre>  T:riga uno  </pre>`) {
		t.Errorf("surrounding whitespace lost:\n%s", out)
	}
}

func TestTranslateHTML_Batches(t *testing.T) {
	api := &stubAPI{}
	c, srv := stubClient(t, api)
	defer srv.Close()

	var b strings.Builder
	b.WriteString("<html><body>")
	const nodes = 300
	for i := 0; i < nodes; i++ {
		fmt.Fprintf(&b, "<p>frase %d</p>\n", i)
	}
	b.WriteString("</body></html>")

	if _, _, err := c.TranslateHTML(context.Background(), b.String()); err != nil {
		t.Fatal(err)
	}
	if got, want := len(api.sent()), nodes; got != want {
		t.Errorf("translated %d segments, want %d", got, want)
	}
	// 300 short segments must go out in ceil(300/128)=3 calls, not 300.
	if got, want := api.calls(), 3; got != want {
		t.Errorf("api calls = %d, want %d", got, want)
	}
}

func TestTranslateHTML_Source(t *testing.T) {
	// Auto-detect (the default): nothing goes out as source, and the language
	// the API detected is reported back for the bar to show.
	api := &stubAPI{}
	c, srv := stubClient(t, api)
	defer srv.Close()
	if _, lang, err := c.TranslateHTML(context.Background(), "<p>ciao</p>"); err != nil || lang != "it" {
		t.Fatalf("lang = %q, err = %v", lang, err)
	}
	if body := api.rawBodies()[0]; strings.Contains(body, `"source"`) {
		t.Errorf("auto-detect sent a source param: %s", body)
	}

	// A picked source is forwarded, and the pair used gets tagged onto the
	// document so the reading pane's pickers can show it.
	api2 := &stubAPI{}
	c2, srv2 := stubClient(t, api2)
	defer srv2.Close()
	c2.Source = "fr"
	out, _, err := c2.TranslateHTML(context.Background(), "<p>bonjour</p>")
	if err != nil {
		t.Fatal(err)
	}
	if body := api2.rawBodies()[0]; !strings.Contains(body, `"source":"fr"`) {
		t.Errorf("source not sent: %s", body)
	}
	for _, want := range []string{`data-mimux-source="fr"`, `data-mimux-target="en"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTranslateHTML_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid target"}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "test-key", Target: "xx", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	if _, _, err := c.TranslateHTML(context.Background(), "<p>ciao</p>"); err == nil {
		t.Fatal("expected error")
	}
}

func TestTranslationCache(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	key := store.TranslationCacheKey("ciao", "", "en")
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
	// Same document and target, different source: a different translation, so
	// it must miss rather than hand back the auto-detected one.
	if _, _, ok, err := s.TranslationCached(store.TranslationCacheKey("ciao", "fr", "en")); err != nil || ok {
		t.Fatalf("source is not part of the cache key: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := s.TranslationCached(store.TranslationCacheKey("ciao", "", "de")); err != nil || ok {
		t.Fatalf("target is not part of the cache key: ok=%v err=%v", ok, err)
	}
}

// stubAPI answers with the Google Translate v2 shape, echoing every segment back
// prefixed with "T:" so assertions can tell translated text from untouched
// markup, and recording what each batch contained.
type stubAPI struct {
	mu      sync.Mutex
	batches [][]string
	raw     []string // request bodies verbatim, so a test can assert what was NOT sent
}

func (a *stubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sent, _ := io.ReadAll(r.Body)
	var body requestBody
	_ = json.Unmarshal(sent, &body)
	a.mu.Lock()
	a.batches = append(a.batches, body.Q)
	a.raw = append(a.raw, string(sent))
	a.mu.Unlock()

	var res apiResponse
	for _, q := range body.Q {
		res.Data.Translations = append(res.Data.Translations, struct {
			TranslatedText     string `json:"translatedText"`
			DetectedSourceLang string `json:"detectedSourceLanguage"`
		}{TranslatedText: "T:" + q, DetectedSourceLang: "it"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (a *stubAPI) sent() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var all []string
	for _, b := range a.batches {
		all = append(all, b...)
	}
	return all
}

func (a *stubAPI) rawBodies() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.raw
}

func (a *stubAPI) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.batches)
}

func stubClient(t *testing.T, api *stubAPI) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(api)
	c := &Client{APIKey: "test-key", Target: "en", HTTPClient: srv.Client()}
	c.HTTPClient.Transport = rewriteHostTransport{base: srv.URL}
	return c, srv
}

// rewriteHostTransport redirects requests to the test server regardless of
// the URL's host, since the client hardcodes the real API host.
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
