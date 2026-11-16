package agent

import (
	"strings"

	"holaclaw/internal/event"
)

// crockford is the ULID alphabet, in ascending value order. Because it is also
// in ascending ASCII order, two ids of equal length compare the same as
// strings and as numbers — which is what makes "the id just above this one"
// well defined without parsing the ULID.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidLen and ulidTimeLen are the canonical text lengths of a ULID and of its
// timestamp prefix.
const (
	ulidLen     = 26
	ulidTimeLen = 10
)

// nextID returns the id immediately above id: the same 48-bit timestamp with
// the 80-bit entropy incremented by one. ok is false when id is not a
// canonical ULID or its entropy is all ones (the increment would carry into
// the timestamp and change the ordering guarantee).
//
// The arithmetic is done on the text: a ULID is a big-endian base-32 number in
// an alphabet whose order matches its values, so incrementing the last
// character with carry is incrementing the number.
func nextID(id event.ID) (event.ID, bool) {
	s := string(id)
	if len(s) != ulidLen {
		return "", false
	}
	b := []byte(s)
	for i := range b {
		if strings.IndexByte(crockford, b[i]) < 0 {
			return "", false
		}
	}
	for i := ulidLen - 1; i >= ulidTimeLen; i-- {
		v := strings.IndexByte(crockford, b[i])
		if v < len(crockford)-1 {
			b[i] = crockford[v+1]
			return event.ID(b), true
		}
		b[i] = crockford[0] // carry
	}
	return "", false // entropy exhausted: would carry into the timestamp
}

// position is a message's place in the transcript: Pos, or the id for a
// message that never went through the store.
func position(m event.Message) event.ID {
	if m.Pos != "" {
		return m.Pos
	}
	return m.ID
}

// chooseCut picks the boundary between the prefix to summarise and the tail to
// keep, and the id the summary message must get.
//
// The tail is the newest whole groups (a tool call is never split from its
// results) starting at a user message, grown until it reaches MinTailTokens;
// it therefore always contains the last user turn and everything after it.
// The smallest such tail is taken, because it reclaims the most.
//
// The summary is stored with the position of the last summarised message plus
// one as its id (and so its position), so it sorts after everything summarised
// and before everything kept. That only works while the two positions differ
// by more than one; when the next one is already taken by the first kept
// message the cut moves one group earlier and the construction is tried again.
//
// cut is the index of the first kept message; ok is false when no boundary
// leaves anything to summarise.
func chooseCut(msgs []event.Message, items []built) (cut int, summaryID event.ID, ok bool) {
	gs := groupMessages(items)
	// starts[i] is the index in msgs of the first message of group i.
	starts := make([]int, len(gs))
	for i, n := 0, 0; i < len(gs); i++ {
		starts[i] = n
		n += len(gs[i].items)
	}

	// Candidate boundaries, oldest first: groups that begin with a user
	// message. A cut anywhere else could orphan a tool result.
	var cands []int
	for i, g := range gs {
		if i > 0 && g.hasUser {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return 0, "", false
	}

	// Walk the candidates newest-first until the tail is big enough, then
	// keep walking older only if the id construction refuses.
	tail := 0
	for k := len(cands) - 1; k >= 0; k-- {
		tail = 0
		for i := cands[k]; i < len(gs); i++ {
			tail += gs[i].tokens
		}
		if tail < MinTailTokens && k > 0 {
			continue
		}
		for j := k; j >= 0; j-- {
			c := repairCut(msgs, starts[cands[j]])
			if c <= 0 || c >= len(msgs) {
				continue
			}
			id, good := nextID(position(msgs[c-1]))
			if !good || id >= position(msgs[c]) {
				continue // adjacent ids: try one group earlier
			}
			return c, id, true
		}
		return 0, "", false
	}
	return 0, "", false
}

// repairCut is the pairing repair as a final pass on the kept tail: a tool
// result whose call is not kept would be an orphan at the head of the prompt,
// so the cut moves past it. A cut at a user message never needs this; the pass
// is kept because the cut is chosen from a grouping of stored rows, which a
// crashed turn can leave in odd shapes.
// A tool result at the head of the kept tail is always an orphan: nothing
// kept precedes it, so its call is in the summarised prefix.
func repairCut(msgs []event.Message, cut int) int {
	for cut < len(msgs) && msgs[cut].Role == event.RoleTool {
		cut++
	}
	return cut
}
