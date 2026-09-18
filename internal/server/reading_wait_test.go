// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/mattmezza/mimux/internal/mail"
)

// TestBodyFetchNoticeNamesTheBusyAccount: "the account may be offline" is the
// wrong story for a fetch that ran out of submitTimeout queued behind a sync
// sweep, and it is the story a big account tells most often.
func TestBodyFetchNoticeNamesTheBusyAccount(t *testing.T) {
	busy := bodyFetchNotice(errors.Join(mail.ErrBusy, errors.New("context deadline exceeded")))
	if !strings.Contains(busy, "busy syncing") {
		t.Errorf("busy notice = %q, want it to name the sync", busy)
	}
	if !strings.Contains(busy, "Try again") {
		t.Errorf("busy notice = %q, want a way forward", busy)
	}

	other := bodyFetchNotice(errors.New("connection refused"))
	if strings.Contains(other, "busy syncing") {
		t.Errorf("generic notice = %q, want no sync talk for a real failure", other)
	}
	if !strings.Contains(other, "offline") {
		t.Errorf("generic notice = %q, want the offline wording", other)
	}
}

// TestReadingPaneSaysWhenItIsWaiting: the pane cannot show an unbounded skeleton
// while a sync holds the connection — it has to say what it is waiting for, and
// offer a real message plus a retry when the fetch gives up. Both live in the
// markup and in the glue that reveals them, so both are asserted here.
func TestReadingPaneSaysWhenItIsWaiting(t *testing.T) {
	skeleton := assetText(t, "templates/pages/inbox.html")
	if !strings.Contains(skeleton, "data-reading-waiting") {
		t.Error("the reading skeleton has nowhere to say it is waiting for a sync")
	}
	if !strings.Contains(skeleton, "Waiting for this account to finish syncing") {
		t.Error("the skeleton's waiting hint is missing its wording")
	}

	detail := assetText(t, "templates/partials/message_detail.html")
	if !strings.Contains(detail, "{{if .Busy}}") {
		t.Error("the reading pane has no busy branch: a timed-out fetch would load an iframe that hits the same queue")
	}
	if !strings.Contains(detail, "This account is busy syncing") {
		t.Error("the busy branch does not say why the message is missing")
	}
	if !strings.Contains(detail, `hx-get="/messages/{{$m.ID}}" hx-target="#reading-pane"`) {
		t.Error("the busy branch has no retry that re-runs the pane")
	}

	app := assetText(t, "static/js/app.js")
	for _, want := range []string{"armReadingWait", "clearReadingWait", "[data-reading-waiting]"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js is missing %s: nothing reveals the waiting hint", want)
		}
	}
}
