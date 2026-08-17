// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"testing"
	"time"
)

// absTime must render the stored-UTC instant as the viewer's wall clock — the
// tooltip is worthless if it disagrees with the clock on the wall.
func TestAbsTime(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("CET", 2*60*60)
	defer func() { time.Local = old }()

	if got := absTime(time.Date(2026, 3, 3, 12, 5, 0, 0, time.UTC)); got != "Tue, 3 Mar 2026, 14:05" {
		t.Errorf("absTime = %q, want %q", got, "Tue, 3 Mar 2026, 14:05")
	}
	if got := absTime(time.Time{}); got != "" {
		t.Errorf("absTime(zero) = %q, want empty", got)
	}
}
