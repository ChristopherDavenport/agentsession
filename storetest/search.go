package storetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testSearch is RFC 0002's Search, as a table any store runs: through
// [agentsession.Search], which is the store's [agentsession.Searcher]
// when it has one, and, for a store that has one, against
// [agentsession.ScanSearch], which it must answer as. Three sessions, one a
// fork: the fork's prefix is found once, in the session that wrote it,
// and a hidden reminder, an instruction layer sent as a message, a
// copied item and an entry off the head's path are not found at all.
func testSearch(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	if _, ok := st.(agentsession.Reader); !ok {
		if _, ok := st.(agentsession.Searcher); !ok {
			t.Skip("the store implements neither agentsession.Reader nor agentsession.Searcher")
		}
	}
	base := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	appendAt := func(id string, min int, e *agentsession.ItemEntry) string {
		t.Helper()
		e.Timestamp = at(min)
		got, err := st.Append(ctx, id, e)
		if err != nil {
			t.Fatalf("append to %s: %v", id, err)
		}
		return got
	}
	user := func(text string) *agentsession.ItemEntry {
		return agentsession.NewItemEntry(openresponses.UserText(text))
	}
	hidden := false

	if _, err := st.Create(ctx, agentsession.Header{ID: "search-a", CreatedAt: at(0), CWD: "/a"}); err != nil {
		t.Fatal(err)
	}
	a1 := appendAt("search-a", 1, user("Deploy the parser"))
	a2 := appendAt("search-a", 2, agentsession.NewItemEntry(openresponses.AssistantText("The parser is deployed.")))
	reminder := user("parser reminder the harness hid")
	reminder.Visible = &hidden
	appendAt("search-a", 3, reminder)
	layer := user("parser rules from AGENTS.md")
	layer.InstructionsPart = &agentsession.LayerRef{ID: "AGENTS.md", Source: agentsession.LayerAgentsMD}
	appendAt("search-a", 4, layer)
	a5 := appendAt("search-a", 5, user("one more parser question"))

	if _, err := st.Create(ctx, agentsession.Header{ID: "search-b", CreatedAt: at(10), CWD: "/b", ParentSession: "search-a", Base: a2}); err != nil {
		t.Fatal(err)
	}
	copied := user("Deploy the parser")
	copied.CopiedFrom = &agentsession.CopiedFrom{Session: "search-a", Entry: a1}
	appendAt("search-b", 11, copied)
	b1 := appendAt("search-b", 12, user("does the forked parser hold"))

	if _, err := st.Create(ctx, agentsession.Header{ID: "search-c", CreatedAt: at(20), CWD: "/c"}); err != nil {
		t.Fatal(err)
	}
	c1 := appendAt("search-c", 21, user("PARSER in session c"))
	appendAt("search-c", 22, agentsession.NewItemEntry(openresponses.AssistantText("nothing to see")))

	cases := []struct {
		name string
		q    agentsession.Query
		want []string
	}{
		{"fork prefix once, hidden and composed and copied not at all",
			agentsession.Query{Text: "parser"}, []string{c1, b1, a5, a2, a1}},
		{"every word, any case",
			agentsession.Query{Text: "  PARSER\tdeploy parser "}, []string{a2, a1}},
		{"a word is a substring", agentsession.Query{Text: "ploy"}, []string{a2, a1}},
		{"no match", agentsession.Query{Text: "parser zebra"}, nil},
		{"roles", agentsession.Query{Text: "parser", Roles: []openresponses.Role{openresponses.RoleAssistant}}, []string{a2}},
		{"empty text is every message",
			agentsession.Query{Roles: []openresponses.Role{openresponses.RoleUser}}, []string{c1, b1, a5, a1}},
		{"exclude", agentsession.Query{Text: "parser", Exclude: []string{"search-c"}}, []string{b1, a5, a2, a1}},
		{"limit", agentsession.Query{Text: "parser", Limit: 2}, []string{c1, b1}},
		{"filter", agentsession.Query{Text: "parser", Filter: agentsession.ListFilter{CWD: "/a"}}, []string{a5, a2, a1}},
		{"filter limit counts searched sessions",
			agentsession.Query{Text: "parser", Exclude: []string{"search-c"}, Filter: agentsession.ListFilter{Limit: 1}}, []string{b1}},
		{"entry time, exclusive",
			agentsession.Query{Text: "parser", After: at(1), Before: at(12)}, []string{a5, a2}},
		{"readable",
			agentsession.Query{Text: "parser", Readable: func(id string) bool { return id != "search-c" }, Filter: agentsession.ListFilter{Limit: 1}},
			[]string{b1}},
	}
	check := func(name string, q agentsession.Query, want []string) {
		t.Helper()
		hits := searchHits(t, ctx, st, q)
		var got []string
		for _, h := range hits {
			got = append(got, h.Entry)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: hits %v, want %v", name, got, want)
		}
		for _, h := range hits {
			if h.Snippet == "" || h.Time.IsZero() || h.Role == "" || !strings.HasPrefix(h.Session, "search-") {
				t.Errorf("%s: hit %+v lacks a member", name, h)
			}
		}
		if _, ok := st.(agentsession.Searcher); ok {
			scan := searchHits(t, ctx, st, q, true)
			if !sameHits(hits, scan) {
				t.Errorf("%s: the store's Searcher answers %v, the scan %v", name, hits, scan)
			}
		}
	}
	for _, c := range cases {
		check(c.name, c.q, c.want)
	}
	hits := searchHits(t, ctx, st, agentsession.Query{Text: "parser", Exclude: []string{"search-b"}})
	if len(hits) == 0 || hits[0].Session != "search-c" || hits[0].Role != openresponses.RoleUser || !hits[0].Time.Equal(at(21)) ||
		!strings.Contains(hits[0].Snippet, "PARSER") {
		t.Errorf("first hit %+v, want search-c's user message at %v", hits, at(21))
	}

	// The head moved back: what it left is off the path.
	if hs, ok := st.(agentsession.HeadStore); ok {
		if err := hs.SetHead(ctx, "search-a", a5, a2); err != nil {
			t.Fatal(err)
		}
		check("after a head move", agentsession.Query{Text: "parser", Filter: agentsession.ListFilter{CWD: "/a"}}, []string{a2, a1})
	}
	// A deleted session is not found.
	if err := st.Delete(ctx, "search-c"); err != nil {
		t.Fatal(err)
	}
	check("after a delete", agentsession.Query{Text: "session c"}, nil)
}

// searchHits collects a search of st, or the scan with scan set.
func searchHits(t *testing.T, ctx context.Context, st agentsession.Store, q agentsession.Query, scan ...bool) []agentsession.Hit {
	t.Helper()
	seq := agentsession.Search(ctx, st, q)
	if len(scan) > 0 && scan[0] {
		seq = agentsession.ScanSearch(ctx, st, q)
	}
	var out []agentsession.Hit
	for h, err := range seq {
		if err != nil {
			t.Fatalf("search %q: %v", q.Text, err)
		}
		out = append(out, h)
	}
	return out
}

// sameHits reports whether two searches found the same messages in the
// same order. Snippets are the store's to cut, so they are not
// compared.
func sameHits(a, b []agentsession.Hit) bool {
	key := func(h agentsession.Hit) string {
		return fmt.Sprintf("%s %s %s %s", h.Session, h.Entry, h.Time.UTC().Format(time.RFC3339Nano), h.Role)
	}
	return slices.EqualFunc(a, b, func(x, y agentsession.Hit) bool { return key(x) == key(y) })
}
