package mail

import "strings"

import "testing"

// mustNotContain fails if any needle appears (case-insensitive) in the output.
func mustNotContain(t *testing.T, out string, needles ...string) {
	t.Helper()
	low := strings.ToLower(out)
	for _, n := range needles {
		if strings.Contains(low, strings.ToLower(n)) {
			t.Errorf("sanitized output must not contain %q\n---\n%s", n, out)
		}
	}
}

func TestSanitizeStripsScriptAndHandlers(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"script tag", `<p>hi</p><script>alert(1)</script>`},
		{"onerror handler", `<img src="cid:x" onerror="alert(1)">`},
		{"onload on body", `<body onload="steal()">x</body>`},
		{"javascript href", `<a href="javascript:alert(1)">click</a>`},
		{"data text/html href", `<a href="data:text/html,<script>alert(1)</script>">click</a>`},
		{"svg script", `<svg><script>alert(1)</script></svg>`},
		{"base tag", `<base href="http://evil.com/"><a href="x">y</a>`},
		{"meta refresh", `<meta http-equiv="refresh" content="0;url=http://evil.com">`},
		{"iframe", `<iframe src="http://evil.com"></iframe>`},
		{"object embed", `<object data="evil.swf"></object><embed src="evil.swf">`},
		{"form", `<form action="http://evil.com"><input name="p"></form>`},
		{"css url exfil", `<div style="background-image:url('http://evil.com/x.png')">hi</div>`},
		{"css expression", `<div style="width:expression(alert(1))">hi</div>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := sanitizeHTML(c.in, nil, false)
			mustNotContain(t, out,
				"<script", "javascript:", "onerror", "onload", "<iframe", "<object",
				"<embed", "<form", "<base", "http-equiv", "evil.com", "expression(")
		})
	}
}

func TestSanitizeBlocksExternalImage(t *testing.T) {
	out, blocked := sanitizeHTML(`<img src="http://tracker.example/pixel.gif">`, nil, false)
	if !blocked {
		t.Fatal("expected external image to be reported as blocked")
	}
	if !strings.Contains(out, `data-ext-src="http://tracker.example/pixel.gif"`) {
		t.Errorf("original src should be stashed in data-ext-src: %s", out)
	}
	if !strings.Contains(out, `src="`+blankGIF+`"`) {
		t.Errorf("blocked image's live src should be the placeholder: %s", out)
	}
}

func TestSanitizeAllowsExternalWhenPermitted(t *testing.T) {
	out, _ := sanitizeHTML(`<img src="http://cdn.example/a.png">`, nil, true)
	if !strings.Contains(out, `src="http://cdn.example/a.png"`) {
		t.Errorf("external src should survive when allowed: %s", out)
	}
}

func TestSanitizeSrcsetNeutralized(t *testing.T) {
	out, blocked := sanitizeHTML(`<img srcset="http://evil.example/a.png 1x, http://evil.example/b.png 2x">`, nil, false)
	if !blocked {
		t.Fatal("expected external srcset to be blocked")
	}
	if !strings.Contains(out, `srcset=""`) {
		t.Errorf("live srcset must be emptied: %s", out)
	}
	if !strings.Contains(out, `data-ext-srcset="http://evil.example/a.png 1x, http://evil.example/b.png 2x"`) {
		t.Errorf("original srcset should be preserved for opt-in: %s", out)
	}
}

func TestSanitizeResolvesCID(t *testing.T) {
	inline := map[string]inlinePart{"logo": {mime: "image/png", data: []byte{1, 2, 3}}}
	out, _ := sanitizeHTML(`<img src="cid:logo">`, inline, false)
	if !strings.Contains(out, "data:image/png;base64,AQID") {
		t.Errorf("cid should resolve to an inline data URI: %s", out)
	}
	mustNotContain(t, out, "cid:")
}

func TestSanitizeUnknownCIDBlanked(t *testing.T) {
	out, _ := sanitizeHTML(`<img src="cid:missing">`, nil, false)
	mustNotContain(t, out, "cid:")
	if !strings.Contains(out, blankGIF) {
		t.Errorf("unknown cid should fall back to placeholder: %s", out)
	}
}

func TestSanitizeMalformedDoesNotPanic(t *testing.T) {
	inputs := []string{
		`<p><b>unclosed`,
		`<<<>>><img src=`,
		`<div><span></div></span>`,
		strings.Repeat("<div>", 500) + "deep" + strings.Repeat("</div>", 500),
		"",
	}
	for _, in := range inputs {
		out, _ := sanitizeHTML(in, nil, false)
		mustNotContain(t, out, "<script")
	}
}

func TestSanitizeKeepsSafeStyles(t *testing.T) {
	out, _ := sanitizeHTML(`<p style="color:#ff0000;font-weight:bold">red</p>`, nil, false)
	if !strings.Contains(out, "color") {
		t.Errorf("safe inline styles should be preserved: %s", out)
	}
}
