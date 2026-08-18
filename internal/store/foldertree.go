// SPDX-License-Identifier: AGPL-3.0-only
package store

import "strings"

// FolderNode is one node in a nested folder tree: a folder-name path segment
// that may itself be a real, selectable folder (Folder != nil) and/or the
// parent of deeper folders (Children). Moved down from internal/server so the
// HTML "Move to…" picker and the pro API's folder listing shape the same tree.
type FolderNode struct {
	Label    string
	Folder   *Folder
	Children []*FolderNode
}

// FolderTree turns a flat, single-account folder list into a nested tree by
// splitting each name on the IMAP hierarchy delimiter ('/' or '.', matching
// FolderLabel). Special-use folders (Inbox, Sent, …) stay as flat top-level
// leaves. Intermediate segments with no folder of their own become
// non-selectable branches. Input order is preserved (folders arrive sorted).
func FolderTree(folders []Folder) []*FolderNode {
	var roots []*FolderNode
	find := func(nodes *[]*FolderNode, label string) *FolderNode {
		for _, n := range *nodes {
			if n.Label == label {
				return n
			}
		}
		n := &FolderNode{Label: label}
		*nodes = append(*nodes, n)
		return n
	}
	for i := range folders {
		f := folders[i]
		var segs []string
		if f.SpecialUse != "" {
			segs = []string{FolderLabel(f)}
		} else {
			segs = strings.FieldsFunc(f.Name, func(r rune) bool { return r == '/' || r == '.' })
		}
		if len(segs) == 0 {
			segs = []string{f.Name}
		}
		cur := &roots
		var node *FolderNode
		for _, s := range segs {
			node = find(cur, s)
			cur = &node.Children
		}
		fc := f
		node.Folder = &fc
	}
	return roots
}

// FolderLabel is the display name of a folder: its capitalized special-use
// role, or the last segment of its IMAP name.
func FolderLabel(f Folder) string {
	if f.SpecialUse != "" {
		return strings.ToUpper(f.SpecialUse[:1]) + f.SpecialUse[1:]
	}
	name := f.Name
	if i := strings.LastIndexAny(name, "/."); i >= 0 {
		name = name[i+1:]
	}
	return name
}
