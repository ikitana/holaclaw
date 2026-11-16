// Package session owns the session layer: the pure session-key function, the
// router that turns an inbound Envelope into a session submission, and the
// actor manager — one goroutine plus a persisted mailbox per conversation,
// strict order inside a conversation, parallel across them (ARCHITECTURE.md §2, docs/sessions.md).
package session

import (
	"strings"

	"holaclaw/internal/event"
)

// Key derives the session key of a conversation. It is the one pure function
// every caller uses (docs/sessions.md):
//
//	dm:       <agent>/<channel>/dm/<user id, else conversation>
//	holaclaw: <agent>/holaclaw/dm/<user id>/<conversation>
//	room:     <agent>/<channel>/room/<conversation>[/<thread>]
//
// Identity-link lookup happens *before* key derivation: the router passes the
// resolved user, so two aliases of one user on one channel instance share a
// session. Every segment is escaped, so a "/" inside a platform id cannot
// forge another key.
//
// The holaclaw clients (the CLI, the apps, automations) open many parallel
// conversations per identity, so there the conversation id is part of the key;
// the identity stays in it so one client can never land in another identity's
// conversation by choosing its id.
func Key(agent string, o event.Origin, identityID event.ID) string {
	segs := make([]string, 0, 5)
	segs = append(segs, agent, o.Channel)
	if o.ConversationKind == event.ConversationRoom {
		segs = append(segs, "room", o.Conversation)
		if o.Thread != "" {
			segs = append(segs, o.Thread)
		}
	} else {
		who := string(identityID)
		if who == "" {
			who = o.Conversation
		}
		segs = append(segs, "dm", who)
		if o.ChannelType == event.ChannelHolaclaw {
			segs = append(segs, o.Conversation)
		}
	}
	return joinKey(segs)
}

// TaskKey is the reserved namespace for detached task runs (docs/tasks.md):
// <agent>/task/<task id>.
func TaskKey(agent string, taskID event.ID) string {
	return joinKey([]string{agent, "task", string(taskID)})
}

func joinKey(segs []string) string {
	var b strings.Builder
	for i, s := range segs {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(escapeSegment(s))
	}
	return b.String()
}

// escapeSegment percent-escapes the two characters that carry meaning in a
// key: the separator and the escape character itself.
func escapeSegment(s string) string {
	if !strings.ContainsAny(s, "/%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%':
			b.WriteString("%25")
		case '/':
			b.WriteString("%2F")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
