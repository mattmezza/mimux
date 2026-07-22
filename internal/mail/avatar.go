package mail

import (
	"crypto/sha256"
	"net/url"
	"strings"
	"unicode"
)

// avatarPalette holds Tailwind-500 hex colors that read well as avatar
// backgrounds on the dark theme.
var avatarPalette = []string{
	"#ef4444", "#f97316", "#f59e0b", "#84cc16", "#22c55e", "#10b981",
	"#14b8a6", "#06b6d4", "#3b82f6", "#6366f1", "#8b5cf6", "#a855f7",
	"#d946ef", "#ec4899", "#f43f5e",
}

// AvatarColor returns a deterministic background color for an email address.
func AvatarColor(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	idx := int(sum[0]) % len(avatarPalette)
	return avatarPalette[idx]
}

// AvatarInitials returns 1–2 uppercase initials from a display name, falling
// back to the email's first letter.
func AvatarInitials(name, email string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		fields := strings.Fields(name)
		switch len(fields) {
		case 1:
			return strings.ToUpper(firstLetter(fields[0]))
		default:
			return strings.ToUpper(firstLetter(fields[0]) + firstLetter(fields[len(fields)-1]))
		}
	}
	if email != "" {
		return strings.ToUpper(firstLetter(email))
	}
	return "?"
}

// FaviconURL returns a Google-hosted favicon URL for the sender's email domain,
// or "" when no usable domain is present. Used as an optional avatar image.
func FaviconURL(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	// Trim any stray angle bracket / whitespace from "Name <a@b.com>" forms.
	domain := strings.ToLower(strings.Trim(strings.TrimSpace(email[at+1:]), "<>"))
	if domain == "" || !strings.Contains(domain, ".") {
		return ""
	}
	return "https://www.google.com/s2/favicons?domain=" + url.QueryEscape(domain) + "&sz=64"
}

func firstLetter(s string) string {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(r)
		}
	}
	return ""
}
