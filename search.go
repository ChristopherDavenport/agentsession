package agentsession

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// Query is a search over a store's sessions (RFC 0002, Search). Zero
// fields do not filter, so the zero query finds every message the
// store's sessions hold.
type Query struct {
	// Text is what to find. It is lowercased and split at white space
	// into words, each distinct word once, and a message matches when
	// every word occurs in its text as a substring: agentmemory RFC
	// 0001's reference match. A Text that is empty, or white space
	// alone, matches every message.
	Text string
	// Filter selects the sessions searched, as it selects what List
	// gives, but for its Limit, which caps the number of sessions
	// searched counted after Exclude and Readable have left theirs out.
	// WithNames does not change a search.
	Filter ListFilter
	// Exclude names sessions to leave out, such as the one a tool
	// searching for its own conversation runs in.
	Exclude []string
	// OmitRedacted leaves out the redacted sessions, those whose header
	// carries redacted (RFC 0001, The redacted session). A store
	// holding a redacted session beside its original holds each message
	// the redaction kept in both, and a search finds it once on each
	// session's own path; with OmitRedacted it finds the original's
	// alone. A session it leaves out does not count toward
	// Filter.Limit.
	OmitRedacted bool
	// Roles keeps only messages of these roles; empty keeps every role.
	Roles []openresponses.Role
	// After and Before bound each message entry's ts, exclusive.
	After, Before time.Time
	// Limit caps the number of hits; zero means no cap.
	Limit int
	// Readable says whether the caller may read a session. A program
	// serving one store to several principals, as RFC 0003's server
	// does, sets it, and a search then finds nothing in a session it
	// refuses, which also does not count toward Filter.Limit. It is
	// asked holding none of the store's locks, so it may read the
	// store. Nil reads every session.
	Readable func(id string) bool
}

// Hit is a message a search found: the session and the entry holding
// it, the entry's ts, the message's role, and a snippet of its text.
type Hit struct {
	Session string
	Entry   string
	Time    time.Time
	Role    openresponses.Role
	// Snippet is a run of the message's text around the first word of
	// the query found in it. The scan's is at most [SnippetRunes] code
	// points; a store answering from an index may cut it otherwise.
	Snippet string
}

// SnippetRunes is the most code points of a message's text a scan's
// [Hit.Snippet] holds.
const SnippetRunes = 200

// snippetLead is how many code points a scan's snippet keeps before the
// match it is cut around.
const snippetLead = 60

// Searcher is implemented by a store that answers a search itself, as
// from an index, faster than [ScanSearch]. Its answer MUST be the scan's
// (RFC 0002, Search): the same messages from the same sessions, in the
// same order, cut at the same limit, a deleted session's never; only
// its matching of the words, and so its snippets, may differ, as the
// store documents, and an index may lag the store's appends and head
// moves, by a bound the store documents, but never a delete. [Search]
// uses it when the store implements it, and the store suite
// (storetest) checks it against ScanSearch, with its SearchOwnMatch
// and SearchSync options for a store that matches otherwise or lags.
type Searcher interface {
	Search(ctx context.Context, q Query) iter.Seq2[Hit, error]
}

// ErrNoReader is returned by [ScanSearch] over a store that does not
// implement [Reader], and so by [Search] over one that implements
// neither Reader nor [Searcher]: a scan reads every session it
// searches, and a store's Open would hold each one.
var ErrNoReader = errors.New("agentsession: the store cannot read a session without holding it")

// Search searches store's sessions: through the store's [Searcher] when
// it implements one, and otherwise by [ScanSearch].
func Search(ctx context.Context, store Store, q Query) iter.Seq2[Hit, error] {
	if s, ok := store.(Searcher); ok {
		return s.Search(ctx, q)
	}
	return ScanSearch(ctx, store, q)
}

// ScanSearch is the reference search (RFC 0002, Search): it lists the
// sessions q.Filter selects, newest first by created_at and by ID
// among those created at one instant, and reads each through the
// store's [Reader]. In each session it searches the message items among
// the session's own entries on the path to its head, from the head
// back: not a fork's prefix, which another session wrote, not an entry
// off the path, not an item marked visible false, not one carrying
// instructions_part, which the harness composed, and not one carrying
// copied_from, which another session's conversation holds. So each
// message a session wrote is found once, in that session: a redacted
// session is a session of its own, and a message its redaction kept is
// found in it and in its original, unless q.OmitRedacted leaves the
// redacted session out. A session deleted between the listing and its
// read is not searched.
func ScanSearch(ctx context.Context, store Store, q Query) iter.Seq2[Hit, error] {
	return func(yield func(Hit, error) bool) {
		r, ok := store.(Reader)
		if !ok {
			yield(Hit{}, ErrNoReader)
			return
		}
		f := q.Filter
		f.Limit, f.WithNames = 0, false
		var sums []Summary
		for sum, err := range store.List(ctx, f) {
			if err != nil {
				yield(Hit{}, err)
				return
			}
			sums = append(sums, sum)
		}
		sort.SliceStable(sums, func(i, j int) bool {
			a, b := sums[i].Header, sums[j].Header
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
			return a.ID < b.ID
		})
		words := queryWords(q.Text)
		hits, searched := 0, 0
		for _, sum := range sums {
			id := sum.Header.ID
			if slices.Contains(q.Exclude, id) || q.OmitRedacted && sum.Header.Redacted || !q.Filter.Keep(sum) {
				continue
			}
			if q.Readable != nil && !q.Readable(id) {
				continue
			}
			if q.Filter.Limit > 0 && searched == q.Filter.Limit {
				return
			}
			if err := ctx.Err(); err != nil {
				yield(Hit{}, err)
				return
			}
			s, err := r.Read(ctx, id)
			if errors.Is(err, ErrNoSession) {
				continue
			}
			if err != nil {
				yield(Hit{}, fmt.Errorf("agentsession: search %s: %w", id, err))
				return
			}
			searched++
			for _, e := range searchable(s) {
				h, ok := q.hit(id, e, words)
				if !ok {
					continue
				}
				if !yield(h, nil) {
					return
				}
				if hits++; q.Limit > 0 && hits == q.Limit {
					return
				}
			}
		}
	}
}

// searchable returns the item entries holding a message that a search
// of s looks at, from the head back.
func searchable(s *Session) []*ItemEntry {
	var out []*ItemEntry
	path := s.Path(s.Leaf())
	for i := len(path) - 1; i >= 0; i-- {
		if s.Prefix(path[i].Base().ID) {
			break
		}
		it, ok := path[i].(*ItemEntry)
		if !ok {
			continue
		}
		if _, ok := it.Item.(*openresponses.Message); !ok {
			continue
		}
		if it.Visible != nil && !*it.Visible || it.Layers() != nil || it.CopiedFrom != nil {
			continue
		}
		out = append(out, it)
	}
	return out
}

// hit returns the hit e makes for q, whose words are given, in the
// session id, and whether it is one.
func (q Query) hit(id string, e *ItemEntry, words []string) (Hit, bool) {
	m := e.Item.(*openresponses.Message)
	if len(q.Roles) > 0 && !slices.Contains(q.Roles, m.Role) {
		return Hit{}, false
	}
	ts := e.Base().Timestamp
	if !q.After.IsZero() && !ts.After(q.After) {
		return Hit{}, false
	}
	if !q.Before.IsZero() && !ts.Before(q.Before) {
		return Hit{}, false
	}
	text := messageText(m)
	lower := strings.ToLower(text)
	for _, w := range words {
		if !strings.Contains(lower, w) {
			return Hit{}, false
		}
	}
	return Hit{Session: id, Entry: e.Base().ID, Time: ts, Role: m.Role, Snippet: snippet(text, lower, words)}, true
}

// queryWords splits a query as agentmemory RFC 0001's reference match
// does: lowercased, each code point by its simple mapping, split at
// white space, each distinct word once.
func queryWords(query string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// messageText is the text a search matches in a message: the text of
// each of its textual parts (input_text, output_text, text and
// refusal), joined by U+000A.
func messageText(m *openresponses.Message) string {
	var parts []string
	for _, c := range m.Content {
		switch p := c.(type) {
		case *openresponses.InputText:
			parts = append(parts, p.Text)
		case *openresponses.OutputText:
			parts = append(parts, p.Text)
		case *openresponses.Text:
			parts = append(parts, p.Text)
		case *openresponses.Refusal:
			parts = append(parts, p.Refusal)
		}
	}
	return strings.Join(parts, "\n")
}

// snippet cuts text to at most SnippetRunes code points around the
// first occurrence of the query's first word, found in lower, text
// lowercased. Lowercasing maps one code point to one, so a position in
// lower is the same position in text.
func snippet(text, lower string, words []string) string {
	rs := []rune(text)
	if len(rs) <= SnippetRunes {
		return text
	}
	start := 0
	if len(words) > 0 {
		if at := strings.Index(lower, words[0]); at > 0 {
			start = max(len([]rune(lower[:at]))-snippetLead, 0)
		}
	}
	start = min(start, len(rs)-SnippetRunes)
	return string(rs[start : start+SnippetRunes])
}
