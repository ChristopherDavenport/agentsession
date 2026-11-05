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
// Three more are created at one instant, out of ID order, one spawned
// and one continued, so that the order among them, the listing's
// filters and a limit on sessions searched, which counts a session
// with no hit, each tell an index from the scan.
func testSearch(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	if _, ok := st.(agentsession.Reader); !ok {
		if _, ok := st.(agentsession.Searcher); !ok {
			t.Skip("the store implements neither agentsession.Reader nor agentsession.Searcher")
		}
	}
	sync := func() {
		if opts.SearchSync != nil {
			opts.SearchSync(t, st)
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
	create := func(h agentsession.Header) {
		t.Helper()
		if _, err := st.Create(ctx, h); err != nil {
			t.Fatalf("create %s: %v", h.ID, err)
		}
	}
	hidden := false

	create(agentsession.Header{ID: "search-a", CreatedAt: at(0), CWD: "/a"})
	a1 := appendAt("search-a", 1, user("Deploy the parser"))
	a2 := appendAt("search-a", 2, agentsession.NewItemEntry(openresponses.AssistantText("The parser is deployed.")))
	reminder := user("parser reminder the harness hid")
	reminder.Visible = &hidden
	appendAt("search-a", 3, reminder)
	layer := user("parser rules from AGENTS.md")
	layer.InstructionsPart = &agentsession.LayerRef{ID: "AGENTS.md", Source: agentsession.LayerAgentsMD}
	appendAt("search-a", 4, layer)
	a5 := appendAt("search-a", 5, user("one more parser question"))

	create(agentsession.Header{ID: "search-b", CreatedAt: at(10), CWD: "/b", ParentSession: "search-a", Base: a2})
	copied := user("Deploy the parser")
	copied.CopiedFrom = &agentsession.CopiedFrom{Session: "search-a", Entry: a1}
	appendAt("search-b", 11, copied)
	b1 := appendAt("search-b", 12, user("does the forked parser hold"))

	create(agentsession.Header{ID: "search-c", CreatedAt: at(20), CWD: "/c"})
	c1 := appendAt("search-c", 21, user("PARSER in session c"))
	appendAt("search-c", 22, agentsession.NewItemEntry(openresponses.AssistantText("nothing to see")))

	// Created at one instant, and not in ID order: t2 is continued in
	// t1, and t3 is a subsession a call of search-a spawned.
	// t0, at the same instant, is a redacted session, which
	// OmitRedacted leaves out.
	create(agentsession.Header{ID: "search-t0", CreatedAt: at(-10), CWD: "/t", Redacted: true})
	create(agentsession.Header{ID: "search-t2", CreatedAt: at(-10), CWD: "/t"})
	create(agentsession.Header{ID: "search-t1", CreatedAt: at(-10), CWD: "/t"})
	create(agentsession.Header{ID: "search-t3", CreatedAt: at(-10), CWD: "/t", ParentSession: "search-a", SpawnedBy: "call_1"})
	t0 := appendAt("search-t0", -9, user("a tie the redaction kept, t0"))
	t1 := appendAt("search-t1", -9, user("a tie at one instant, t1"))
	t2 := appendAt("search-t2", -9, user("a tie at one instant, t2"))
	t3 := appendAt("search-t3", -9, user("a tie at one instant, t3"))
	if _, err := st.Append(ctx, "search-t2", agentsession.NewLinkEntry(agentsession.RelContinuedIn, "search-t1")); err != nil {
		t.Fatal(err)
	}
	sync()

	// readable reads the store while it answers, as a server's read
	// check may: a Searcher holding its own lock while it asks would
	// stall here.
	readable := func(id string) bool {
		for _, err := range st.List(ctx, agentsession.ListFilter{}) {
			if err != nil {
				t.Errorf("List inside Readable: %v", err)
			}
		}
		if r, ok := st.(agentsession.Reader); ok {
			if _, err := r.Read(ctx, id); err != nil {
				t.Errorf("Read inside Readable: %v", err)
			}
		}
		return id != "search-c"
	}
	cases := []struct {
		name string
		q    agentsession.Query
		want []string
		// ref holds the store to the reference match.
		ref bool
	}{
		{"fork prefix once, hidden and composed and copied not at all",
			agentsession.Query{Text: "parser"}, []string{c1, b1, a5, a2, a1}, false},
		{"every word, any case",
			agentsession.Query{Text: "  PARSER\tdeploy parser "}, []string{a2, a1}, true},
		{"a word is a substring", agentsession.Query{Text: "ploy"}, []string{a2, a1}, true},
		{"no match", agentsession.Query{Text: "parser zebra"}, nil, false},
		{"roles", agentsession.Query{Text: "parser", Roles: []openresponses.Role{openresponses.RoleAssistant}}, []string{a2}, false},
		{"empty text is every message",
			agentsession.Query{Roles: []openresponses.Role{openresponses.RoleUser}}, []string{c1, b1, a5, a1, t0, t1, t2, t3}, false},
		{"exclude", agentsession.Query{Text: "parser", Exclude: []string{"search-c"}}, []string{b1, a5, a2, a1}, false},
		{"limit", agentsession.Query{Text: "parser", Limit: 2}, []string{c1, b1}, false},
		{"filter", agentsession.Query{Text: "parser", Filter: agentsession.ListFilter{CWD: "/a"}}, []string{a5, a2, a1}, false},
		{"filter limit counts searched sessions",
			agentsession.Query{Text: "parser", Exclude: []string{"search-c"}, Filter: agentsession.ListFilter{Limit: 1}}, []string{b1}, false},
		{"filter limit counts a searched session with no hit",
			agentsession.Query{Text: "deploy", Filter: agentsession.ListFilter{Limit: 1}}, nil, false},
		{"sessions created at one instant, by ID", agentsession.Query{Text: "tie"}, []string{t0, t1, t2, t3}, false},
		{"filter limit among sessions created at one instant",
			agentsession.Query{Text: "tie", Filter: agentsession.ListFilter{CWD: "/t", Limit: 2}}, []string{t0, t1}, false},
		{"omit redacted, which counts toward no limit",
			agentsession.Query{Text: "tie", OmitRedacted: true, Filter: agentsession.ListFilter{CWD: "/t", Limit: 1}}, []string{t1}, false},
		{"parent session",
			agentsession.Query{Roles: []openresponses.Role{openresponses.RoleUser}, Filter: agentsession.ListFilter{ParentSession: "search-a"}},
			[]string{b1, t3}, false},
		{"top level", agentsession.Query{Text: "tie", Filter: agentsession.ListFilter{TopLevel: true}}, []string{t0, t1, t2}, false},
		{"current", agentsession.Query{Text: "tie", Filter: agentsession.ListFilter{Current: true}}, []string{t0, t1, t3}, false},
		{"entry time, exclusive",
			agentsession.Query{Text: "parser", After: at(1), Before: at(12)}, []string{a5, a2}, false},
		{"readable, which reads the store",
			agentsession.Query{Text: "parser", Readable: readable, Filter: agentsession.ListFilter{Limit: 1}}, []string{b1}, false},
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
		if c.ref && opts.SearchOwnMatch {
			continue
		}
		check(c.name, c.q, c.want)
	}
	hits := searchHits(t, ctx, st, agentsession.Query{Text: "parser", Exclude: []string{"search-b"}})
	if len(hits) == 0 || hits[0].Session != "search-c" || hits[0].Role != openresponses.RoleUser || !hits[0].Time.Equal(at(21)) ||
		!opts.SearchOwnMatch && !strings.Contains(hits[0].Snippet, "PARSER") {
		t.Errorf("first hit %+v, want search-c's user message at %v", hits, at(21))
	}

	// The head moved back: what it left is off the path.
	if hs, ok := st.(agentsession.HeadStore); ok {
		if err := hs.SetHead(ctx, "search-a", a5, a2); err != nil {
			t.Fatal(err)
		}
		sync()
		check("after a head move", agentsession.Query{Text: "parser", Filter: agentsession.ListFilter{CWD: "/a"}}, []string{a2, a1})
	}
	// A deleted session is not found, with no wait for an index to
	// catch up: no search lags a delete.
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
