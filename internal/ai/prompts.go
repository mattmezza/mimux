package ai

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed prompts/*.tmpl
var promptsFS embed.FS

var prompts = template.Must(template.ParseFS(promptsFS, "prompts/*.tmpl"))

func systemPrompt() string {
	var buf bytes.Buffer
	if err := prompts.ExecuteTemplate(&buf, "system.tmpl", nil); err != nil {
		panic(err) // static template, only fails on programmer error
	}
	return buf.String()
}

func composePrompt(topic string) (string, error) {
	var buf bytes.Buffer
	err := prompts.ExecuteTemplate(&buf, "compose.tmpl", map[string]string{"Topic": topic})
	return buf.String(), err
}

func replyPrompt(threadContext, instructions string) (string, error) {
	var buf bytes.Buffer
	err := prompts.ExecuteTemplate(&buf, "reply.tmpl", map[string]string{
		"ThreadContext": threadContext,
		"Instructions":  instructions,
	})
	return buf.String(), err
}
