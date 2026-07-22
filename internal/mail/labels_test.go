package mail

import (
	"reflect"
	"testing"
)

func TestMessageLabels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{`\Inbox \Important`, nil},                              // system labels dropped
		{`\Inbox "Work" "Receipts"`, []string{"Work", "Receipts"}}, // user labels kept, quotes stripped
		{`Personal Personal`, []string{"Personal"}},            // deduped
		{`\Sent Project_X`, []string{"Project X"}},             // underscore→space
		{`\Starred Unread`, nil},                               // "Unread" is a system name after normalize
	}
	for _, c := range cases {
		if got := MessageLabels(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("MessageLabels(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
