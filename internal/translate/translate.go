// SPDX-License-Identifier: AGPL-3.0-only
// Package translate calls the Google Translate v2 REST API. Callers cache the
// results in SQLite (see store.TranslationCacheKey).
package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ErrDisabled is returned when no API key is configured.
var ErrDisabled = errors.New("translate: disabled (no api key configured)")

const apiURL = "https://translation.googleapis.com/language/translate/v2"

// Batching limits for one API call. Google v2 accepts repeated q values; the
// documented ceiling is 128 segments and it starts rejecting very large bodies,
// so a long email goes out as a handful of requests instead of one per node.
// maxRequests caps what a single message can cost — beyond it the tail of a
// giant newsletter stays in its original language.
// NOTE: fixed caps, no adaptive splitting. Revisit only if real messages
// start hitting maxRequests.
const (
	maxSegments = 128
	maxChars    = 5000
	maxRequests = 20
)

// skipTags never hold human-readable prose. <head> covers <style>/<title> too.
var skipTags = map[string]bool{"script": true, "style": true, "head": true, "noscript": true}

// Client talks to the Google Translate v2 API.
type Client struct {
	APIKey     string
	Target     string
	Source     string // empty = let the API auto-detect (and report what it found)
	HTTPClient *http.Client
}

// NewClient builds a Client with a sane default timeout.
func NewClient(apiKey, target string) *Client {
	return &Client{
		APIKey:     apiKey,
		Target:     target,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type requestBody struct {
	Q      []string `json:"q"`
	Target string   `json:"target"`
	Source string   `json:"source,omitempty"` // omitted = auto-detect
	Format string   `json:"format"`
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

// textSeg is one translatable text node plus the surrounding whitespace, which
// the API strips and which matters inside <pre> and between inline tags.
type textSeg struct {
	node        *html.Node
	lead, trail string
	text        string
}

// TranslateHTML translates the human-readable text of an HTML document and
// returns the re-serialized document — same markup, styling, images and
// layout, only the words change. The <html> element is tagged with the pair
// that was used — data-mimux-target, data-mimux-source when the caller picked one,
// and data-mimux-lang=<detected source language> when it did not — so the reading
// pane can label it.
func (c *Client) TranslateHTML(ctx context.Context, doc string) (out, detectedLang string, err error) {
	if c == nil || c.APIKey == "" {
		return "", "", ErrDisabled
	}
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return "", "", fmt.Errorf("translate: parse html: %w", err)
	}
	segs := collectText(root)
	if len(segs) == 0 {
		return doc, "", nil
	}

	requests := 0
	for i := 0; i < len(segs) && requests < maxRequests; requests++ {
		j, n := i, 0
		for j < len(segs) && j-i < maxSegments && (j == i || n+len(segs[j].text) <= maxChars) {
			n += len(segs[j].text)
			j++
		}
		q := make([]string, 0, j-i)
		for _, s := range segs[i:j] {
			q = append(q, s.text)
		}
		got, lang, err := c.translateBatch(ctx, q)
		if err != nil {
			return "", "", err
		}
		if detectedLang == "" {
			detectedLang = lang
		}
		for k, s := range segs[i:j] {
			// format=text still comes back HTML-escaped; html.Render re-escapes
			// whatever we store, so unescape once here.
			s.node.Data = s.lead + html.UnescapeString(got[k]) + s.trail
		}
		i = j
	}

	// The reading pane's bar paints itself from these: which pair was used
	// (so the two pickers show it) and, when auto-detecting, what came back.
	if el := findElement(root, "html"); el != nil {
		if detectedLang != "" {
			el.Attr = append(el.Attr, html.Attribute{Key: "data-mimux-lang", Val: detectedLang})
		}
		if c.Source != "" {
			el.Attr = append(el.Attr, html.Attribute{Key: "data-mimux-source", Val: c.Source})
		}
		el.Attr = append(el.Attr, html.Attribute{Key: "data-mimux-target", Val: c.Target})
	}
	var buf bytes.Buffer
	if err := html.Render(&buf, root); err != nil {
		return "", "", fmt.Errorf("translate: render html: %w", err)
	}
	return buf.String(), detectedLang, nil
}

// collectText walks the tree in document order and returns every text node that
// carries actual words, skipping whitespace-only nodes and script/style/head.
func collectText(root *html.Node) []textSeg {
	var segs []textSeg
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			if lead, text, trail := splitWS(n.Data); text != "" {
				segs = append(segs, textSeg{node: n, lead: lead, text: text, trail: trail})
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(root)
	return segs
}

func splitWS(s string) (lead, text, trail string) {
	t := strings.TrimLeft(s, " \t\r\n")
	lead = s[:len(s)-len(t)]
	text = strings.TrimRight(t, " \t\r\n")
	return lead, text, t[len(text):]
}

func findElement(n *html.Node, name string) *html.Node {
	if n.Type == html.ElementNode && n.Data == name {
		return n
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if found := findElement(ch, name); found != nil {
			return found
		}
	}
	return nil
}

// translateBatch translates q in one call. The API answers in input order, so
// the results line up index by index.
func (c *Client) translateBatch(ctx context.Context, q []string) (out []string, detectedLang string, err error) {
	if c == nil || c.APIKey == "" {
		return nil, "", ErrDisabled
	}
	body, err := json.Marshal(requestBody{Q: q, Target: c.Target, Source: c.Source, Format: "text"})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"?key="+c.APIKey, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("translate: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("translate: read response: %w", err)
	}
	var res apiResponse
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, "", fmt.Errorf("translate: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if res.Error != nil && res.Error.Message != "" {
			return nil, "", fmt.Errorf("translate: api error: %s", res.Error.Message)
		}
		return nil, "", fmt.Errorf("translate: api returned status %d", resp.StatusCode)
	}
	if len(res.Data.Translations) != len(q) {
		return nil, "", fmt.Errorf("translate: got %d translations for %d segments", len(res.Data.Translations), len(q))
	}
	out = make([]string, len(q))
	for i, t := range res.Data.Translations {
		out[i] = t.TranslatedText
		if detectedLang == "" {
			detectedLang = t.DetectedSourceLang
		}
	}
	return out, detectedLang, nil
}
