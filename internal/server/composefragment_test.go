// SPDX-License-Identifier: AGPL-3.0-only
package server

import "testing"

// TestMailtoPrefill: what a mailto: link handed over by the browser is allowed
// to put in the compose window — and what it isn't.
func TestMailtoPrefill(t *testing.T) {
	cases := []struct {
		name, in                   string
		to, cc, bcc, subject, body string
	}{
		{"plain", "mailto:a@x.com", "a@x.com", "", "", "", ""},
		{"encoded", "mailto:a%40x.com", "a@x.com", "", "", "", ""},
		{"list", "mailto:a@x.com,b@x.com", "a@x.com, b@x.com", "", "", "", ""},
		{"to param only", "mailto:?to=a@x.com", "a@x.com", "", "", "", ""},
		{"both halves merge", "mailto:a@x.com?to=b@x.com", "a@x.com, b@x.com", "", "", "", ""},
		{"plus is not a space", "mailto:a+tag@x.com", "a+tag@x.com", "", "", "", ""},
		{"subject and body", "mailto:a@x.com?subject=hi%20there%20%26%20bye&body=one%0Atwo%20%26%20three",
			"a@x.com", "", "", "hi there & bye", "one\ntwo & three"},
		{"cc and bcc", "mailto:a@x.com?cc=c@x.com&bcc=b@x.com", "a@x.com", "c@x.com", "b@x.com", "", ""},
		{"other headers ignored", "mailto:a@x.com?from=evil@x.com&reply-to=evil@x.com&x-mailer=nope&subject=ok",
			"a@x.com", "", "", "ok", ""},
		{"wrong scheme", "https://evil.example/?subject=no", "", "", "", "", ""},
		{"garbage", "%%%not a url at all%%%", "", "", "", "", ""},
		{"empty", "", "", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v composeView
			mailtoPrefill(&v, c.in)
			if v.To != c.to || v.Cc != c.cc || v.Bcc != c.bcc || v.Subject != c.subject || v.Body != c.body {
				t.Errorf("mailtoPrefill(%q) = to:%q cc:%q bcc:%q subject:%q body:%q; want to:%q cc:%q bcc:%q subject:%q body:%q",
					c.in, v.To, v.Cc, v.Bcc, v.Subject, v.Body, c.to, c.cc, c.bcc, c.subject, c.body)
			}
		})
	}

	// The body of a mailto: is text and comes from anywhere at all; the WYSIWYG
	// editor renders its value as markup, so it must arrive escaped.
	v := composeView{Mode: "html"}
	mailtoPrefill(&v, "mailto:a@x.com?body=%3Cimg%20src%3Dx%20onerror%3Dalert(1)%3E%0Ahi")
	if want := "&lt;img src=x onerror=alert(1)&gt;<br>hi"; v.Body != want {
		t.Errorf("html-mode body = %q; want %q", v.Body, want)
	}
}

// TestComposeFragment: the typeahead must query only the token being typed —
// earlier, already-accepted addresses in the field are not part of the query.
func TestComposeFragment(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"al":                          "al",
		"  al  ":                      "al",
		"bob@x.com, al":               "al",
		"bob@x.com,al":                "al",
		"Bob <bob@x.com>, ":           "",
		"Bob <bob@x.com>, Alice <ali": "ali",
		"Alice <ali>":                 "ali",
		"a@x.com, b@x.com, c":         "c",
	}
	for in, want := range cases {
		if got := composeFragment(in); got != want {
			t.Errorf("composeFragment(%q) = %q; want %q", in, got, want)
		}
	}
}
