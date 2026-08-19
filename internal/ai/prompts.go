// SPDX-License-Identifier: AGPL-3.0-only
package ai

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed prompts/*.tmpl
var promptsFS embed.FS

var prompts = template.Must(template.ParseFS(promptsFS, "prompts/*.tmpl"))

func exec(name string, data any) (string, error) {
	var buf bytes.Buffer
	err := prompts.ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

// systemPrompt is the format/tone/brevity/language-aware instruction shared by
// the draft and refine calls.
func systemPrompt(p Prefs, format string) string {
	s, err := exec("system.tmpl", map[string]string{
		"Format":   format,
		"Tone":     p.Tone,
		"Brevity":  p.Brevity,
		"Language": p.Language,
	})
	if err != nil {
		panic(err) // static template, only fails on programmer error
	}
	return s
}

func optionsPrompt(threadContext string, n int) (string, error) {
	return exec("options.tmpl", map[string]any{"ThreadContext": threadContext, "N": n})
}

func draftPrompt(threadContext, direction string, wantSubject bool) (string, error) {
	return exec("draft.tmpl", map[string]any{
		"ThreadContext": threadContext,
		"Direction":     direction,
		"WantSubject":   wantSubject,
	})
}

func refinePrompt(text, action string) (string, error) {
	return exec("refine.tmpl", map[string]string{"Text": text, "Action": action})
}

func summarizePrompt(body, level, language string, truncated, thread bool) (string, error) {
	return exec("summarize.tmpl", map[string]any{
		"Body":      body,
		"Level":     level,
		"Language":  language,
		"Truncated": truncated,
		"Thread":    thread,
	})
}
