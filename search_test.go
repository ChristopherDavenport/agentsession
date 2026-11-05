package agentsession_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// searchOne searches a store holding one session of the given messages
// and returns the hits.
func searchOne(t *testing.T, q agentsession.Query, msgs ...*openresponses.Message) []agentsession.Hit {
	t.Helper()
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(m)); err != nil {
			t.Fatal(err)
		}
	}
	var out []agentsession.Hit
	for h, err := range agentsession.Search(ctx, st, q) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// The reference match is agentmemory RFC 0001's: lowercased by the
// simple mapping, split at Unicode white space, every word a substring
// of the text, whose parts are joined by a line feed.
func TestSearchReferenceMatch(t *testing.T) {
	parts := &openresponses.Message{Role: openresponses.RoleAssistant, Content: openresponses.Contents{
		&openresponses.OutputText{Text: "first part"}, &openresponses.Refusal{Refusal: "cannot comply"}}}
	for _, c := range []struct {
		query string
		msg   *openresponses.Message
		want  bool
	}{
		{"İstanbul", openresponses.UserText("istanbul trip"), true},
		{"ΣΟΦΟΣ", openresponses.UserText("σοφοσ"), true},
		{"trip　istanbul", openresponses.UserText("İstanbul trip"), true},
		{"part cannot", parts, true},
		{"partcannot", parts, false},
		{"part\ncannot", parts, true},
		{"comply", parts, true},
		{"", openresponses.UserText("anything"), true},
		{"  \t", openresponses.UserText("anything"), true},
		{"istanbul zebra", openresponses.UserText("istanbul"), false},
	} {
		if got := len(searchOne(t, agentsession.Query{Text: c.query}, c.msg)) == 1; got != c.want {
			t.Errorf("%q in %q: %v, want %v", c.query, c.msg.Text(), got, c.want)
		}
	}
}

// A snippet is the whole text when it is short, and otherwise a run of
// SnippetRunes code points around the first word found.
func TestSearchSnippet(t *testing.T) {
	long := strings.Repeat("é", 500) + "NEEDLE" + strings.Repeat("ü", 500)
	hits := searchOne(t, agentsession.Query{Text: "needle"}, openresponses.UserText(long))
	if len(hits) != 1 {
		t.Fatalf("hits %v", hits)
	}
	sn := hits[0].Snippet
	if n := len([]rune(sn)); n != agentsession.SnippetRunes || !strings.Contains(sn, "NEEDLE") || !strings.Contains(long, sn) {
		t.Errorf("snippet of %d code points: %q", n, sn)
	}
	if hits := searchOne(t, agentsession.Query{Text: "short"}, openresponses.UserText("a short one")); len(hits) != 1 || hits[0].Snippet != "a short one" {
		t.Errorf("short snippet: %v", hits)
	}
	// Lowercasing changes a code point's length in bytes (İ to i, Ⱥ to
	// ⱥ), never the count of code points before the match.
	mixed := strings.Repeat("İ", 300) + "NEEDLE" + strings.Repeat("Ⱥ", 300)
	hits = searchOne(t, agentsession.Query{Text: "needle"}, openresponses.UserText(mixed))
	want := strings.Repeat("İ", 60) + "NEEDLE" + strings.Repeat("Ⱥ", agentsession.SnippetRunes-66)
	if len(hits) != 1 || hits[0].Snippet != want {
		t.Errorf("snippet across a change of length: %v", hits)
	}
	head := searchOne(t, agentsession.Query{}, openresponses.UserText(long))
	if len(head) != 1 || head[0].Snippet != string([]rune(long)[:agentsession.SnippetRunes]) {
		t.Errorf("an empty query's snippet is not the text's start: %v", head)
	}
}

// listOnly is a store with no Reader: a scan would have to Open, and
// hold, each session.
type listOnly struct{ agentsession.Store }

func TestSearchNeedsAReader(t *testing.T) {
	for _, err := range agentsession.Search(context.Background(), listOnly{agentsession.NewMemoryStore()}, agentsession.Query{}) {
		if !errors.Is(err, agentsession.ErrNoReader) {
			t.Errorf("search of a store without Reader: %v, want ErrNoReader", err)
		}
		return
	}
	t.Error("search of a store without Reader yielded nothing")
}

// An item carrying instructions_part or copied_from in a form the
// format does not define is left out all the same: the member says the
// harness composed it or another session holds it, whatever its form.
func TestSearchLeavesOutAMemberInAnotherForm(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"instructions_part", "copied_from"} {
		e := agentsession.NewItemEntry(openresponses.UserText("needle under " + member))
		e.Unknown = map[string]json.RawMessage{member: json.RawMessage(`"not the format's form"`)}
		if _, err := st.Append(ctx, s.ID(), e); err != nil {
			t.Fatalf("append with %s in another form: %v", member, err)
		}
	}
	for h, err := range agentsession.Search(ctx, st, agentsession.Query{Text: "needle"}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("found %q", h.Snippet)
	}
}

// Once Filter.Limit sessions are searched, no other session's read
// check is asked: it may be a round trip to an authorizer.
func TestSearchAsksNoReadCheckPastTheLimit(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	for i := range 3 {
		if _, err := st.Create(ctx, agentsession.Header{CreatedAt: time.Date(2026, 11, 4, i, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	var asked []string
	q := agentsession.Query{Filter: agentsession.ListFilter{Limit: 1}, Readable: func(id string) bool { asked = append(asked, id); return true }}
	for _, err := range agentsession.Search(ctx, st, q) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(asked) != 1 {
		t.Errorf("read checks asked: %v, want the one session searched", asked)
	}
}
