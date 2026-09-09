// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"testing"
	"time"
)

func TestDayLabel(t *testing.T) {
	now := time.Now().Local()
	if got := dayLabel(now); got != "Today" {
		t.Fatalf("today = %q", got)
	}
	yesterday := now.AddDate(0, 0, -1)
	if got := dayLabel(yesterday); got != "Yesterday" {
		t.Fatalf("yesterday = %q", got)
	}
	old := time.Date(2020, time.June, 15, 12, 0, 0, 0, time.Local)
	if got := dayLabel(old); got != "15 Jun" {
		t.Fatalf("old date = %q", got)
	}
	if got := dayKey(old); got != "2020-06-15" {
		t.Fatalf("key = %q", got)
	}
}
