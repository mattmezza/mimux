// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"testing"

	"github.com/mattmezza/mimux/internal/store"
)

func TestFolderTree(t *testing.T) {
	folders := []store.Folder{
		{ID: 1, Name: "INBOX", SpecialUse: "inbox"},
		{ID: 2, Name: "Work/Clients/Acme"},
		{ID: 3, Name: "Work/Clients"},
		{ID: 4, Name: "Work/Internal"},
		{ID: 5, Name: "Receipts"},
	}
	roots := folderTree(folders)
	// Roots: Inbox (special leaf), Work (branch, no own folder), Receipts (leaf).
	if len(roots) != 3 {
		t.Fatalf("want 3 roots, got %d", len(roots))
	}
	work := roots[1]
	if work.Label != "Work" || work.Folder != nil {
		t.Fatalf("Work should be a non-selectable branch, got label=%q folder=%v", work.Label, work.Folder)
	}
	// Work has children Clients (branch that is ALSO a real folder id=3) and Internal.
	if len(work.Children) != 2 {
		t.Fatalf("Work want 2 children, got %d", len(work.Children))
	}
	clients := work.Children[0]
	if clients.Label != "Clients" || clients.Folder == nil || clients.Folder.ID != 3 {
		t.Fatalf("Clients should be a real folder id=3 with a child, got %+v", clients)
	}
	if len(clients.Children) != 1 || clients.Children[0].Folder.ID != 2 {
		t.Fatalf("Clients should have Acme(id=2) child, got %+v", clients.Children)
	}
}
