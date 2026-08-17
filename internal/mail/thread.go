package mail

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// Thread is a group of messages that form one conversation, ordered oldest
// first (newest last, per the reading-view spec).
type Thread struct {
	Messages     []store.Message
	Subject      string
	Participants []string
	Count        int
	Latest       time.Time
	Unread       bool
	Starred      bool
	// Total is the conversation's size across ALL folders (store.ConversationSizes),
	// set by the list handler only; 0 means "not known here".
	Total int
}

// Size is what the UI shows as the conversation's message count: the whole
// conversation when the caller filled Total in, else just this Thread's members
// (Count keeps meaning "messages in this Thread struct").
func (t Thread) Size() int {
	if t.Total > t.Count {
		return t.Total
	}
	return t.Count
}

// Latest message in the thread (the one a reply targets).
func (t Thread) LatestMessage() store.Message { return t.Messages[len(t.Messages)-1] }

// RootID is the thread's stable list key: the id of its latest message.
func (t Thread) RootID() int64 { return t.Messages[len(t.Messages)-1].ID }

// BuildThreads groups a folder's messages into conversations using JWZ
// threading over References/In-Reply-To. Messages that carry no threading
// headers stand alone — same as gmail.com. Messages carrying a Gmail thread id
// (X-GM-THRID) are grouped strictly by it. Pure and in-memory: no I/O, no
// schema dependency beyond the message rows passed in.
func BuildThreads(msgs []store.Message) []Thread {
	var jwzMsgs []store.Message
	byThrid := map[string][]store.Message{}
	var thridOrder []string
	for _, m := range msgs {
		if m.GmThrID != "" {
			// X-GM-THRID is only unique within one mailbox: scope it by account
			// so two accounts' thread ids can never collide in the unified view.
			key := m.Account + "\x00" + m.GmThrID
			if _, ok := byThrid[key]; !ok {
				thridOrder = append(thridOrder, key)
			}
			byThrid[key] = append(byThrid[key], m)
		} else {
			jwzMsgs = append(jwzMsgs, m)
		}
	}

	var threads []Thread
	for _, id := range thridOrder {
		threads = append(threads, makeThread(byThrid[id]))
	}
	threads = append(threads, jwzThreads(jwzMsgs)...)

	sort.SliceStable(threads, func(i, j int) bool { return threads[i].Latest.After(threads[j].Latest) })
	return threads
}

// --- JWZ container algorithm ---

type container struct {
	id       string
	msg      *store.Message
	parent   *container
	children []*container
}

func (c *container) addChild(child *container) {
	if child.parent != nil {
		child.parent.removeChild(child)
	}
	child.parent = c
	c.children = append(c.children, child)
}

func (c *container) removeChild(child *container) {
	for i, ch := range c.children {
		if ch == child {
			c.children = append(c.children[:i], c.children[i+1:]...)
			return
		}
	}
}

// hasDescendant reports whether other is c or lies in c's subtree. Used as the
// loop guard: never link two containers if it would form a cycle.
func (c *container) hasDescendant(other *container) bool {
	if c == other {
		return true
	}
	for _, ch := range c.children {
		if ch.hasDescendant(other) {
			return true
		}
	}
	return false
}

func jwzThreads(msgs []store.Message) []Thread {
	if len(msgs) == 0 {
		return nil
	}
	idTable := map[string]*container{}
	var all []*container
	synthetic := 0

	getContainer := func(rawID string) *container {
		id := strings.Trim(strings.TrimSpace(rawID), "<> ")
		if id == "" {
			return nil
		}
		c := idTable[id]
		if c == nil {
			c = &container{id: id}
			idTable[id] = c
			all = append(all, c)
		}
		return c
	}

	for i := range msgs {
		m := &msgs[i]
		mc := getContainer(m.MessageID)
		if mc == nil || mc.msg != nil {
			// Missing or duplicate Message-ID: give it its own container so two
			// unrelated messages never collapse into one.
			synthetic++
			mc = &container{id: fmt.Sprintf("\x00syn-%d", synthetic)}
			all = append(all, mc)
		}
		mc.msg = m

		// Reference chain: References is preferred; In-Reply-To only when
		// References is absent (References wins on conflict).
		refIDs := splitMsgIDs(m.Refs)
		if len(refIDs) == 0 {
			refIDs = splitMsgIDs(m.InReplyTo)
		}
		var prev *container
		for _, rid := range refIDs {
			cur := getContainer(rid)
			if cur == nil {
				continue
			}
			if prev != nil && cur.parent == nil && cur != prev && !cur.hasDescendant(prev) {
				prev.addChild(cur)
			}
			prev = cur
		}
		if prev != nil && prev != mc && !mc.hasDescendant(prev) {
			prev.addChild(mc)
		}
	}

	// Collect the root set under a virtual root, then prune empty containers.
	root := &container{id: "\x00root"}
	for _, c := range all {
		if c.parent == nil {
			root.addChild(c)
		}
	}
	pruneEmpty(root)

	// Flatten each pruned root to a flat message group.
	var groups [][]store.Message
	for _, c := range root.children {
		var g []store.Message
		flatten(c, &g)
		if len(g) > 0 {
			groups = append(groups, g)
		}
	}

	// No subject fallback: Message-ID/References linkage is the only signal.
	// Gmail doesn't thread header-less mail that merely shares a subject, and
	// merging on subject alone collapsed unrelated notification mail (a month of
	// "New reasons prevent pages from being indexed on site X") into one row,
	// making the list disagree with gmail.com. NOTE: exact-subject grouping
	// is what X-GM-THRID would replace anyway — see store.Message.GmThrID.
	out := make([]Thread, 0, len(groups))
	for _, g := range groups {
		out = append(out, makeThread(g))
	}
	return out
}

// pruneEmpty removes empty containers and promotes lone children, JWZ-style.
// An empty container with multiple children is kept as a grouping node.
func pruneEmpty(parent *container) {
	for _, c := range append([]*container(nil), parent.children...) {
		pruneEmpty(c)
		switch {
		case c.msg == nil && len(c.children) == 0:
			parent.removeChild(c)
		case c.msg == nil && len(c.children) == 1:
			child := c.children[0]
			c.removeChild(child)
			parent.removeChild(c)
			parent.addChild(child)
		}
	}
}

func flatten(c *container, out *[]store.Message) {
	if c.msg != nil {
		*out = append(*out, *c.msg)
	}
	for _, ch := range c.children {
		flatten(ch, out)
	}
}

// threadSubject returns the subject of the earliest message in a group.
func threadSubject(g []store.Message) string {
	earliest := g[0]
	for _, m := range g[1:] {
		if m.Date.Before(earliest.Date) {
			earliest = m
		}
	}
	return earliest.Subject
}

func makeThread(msgs []store.Message) Thread {
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Date.Before(msgs[j].Date) })
	t := Thread{Messages: msgs, Count: len(msgs), Subject: threadSubject(msgs)}
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Date.After(t.Latest) {
			t.Latest = m.Date
		}
		if !m.IsRead {
			t.Unread = true
		}
		if m.IsStarred {
			t.Starred = true
		}
		name := m.FromName
		if name == "" {
			name = m.FromAddress
		}
		if name != "" && !seen[name] {
			seen[name] = true
			t.Participants = append(t.Participants, name)
		}
	}
	return t
}
