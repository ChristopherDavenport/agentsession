package agentsession_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
