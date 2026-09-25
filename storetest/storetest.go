// Package storetest is the conformance suite for [agentsession.Store]
// implementations. Every store in this repository runs it; a store
// elsewhere can too.
package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// Options configure a run.
type Options struct {
	// New returns a fresh, empty store.
	New func(t *testing.T) agentsession.Store
	// Reopen returns a second handle on the same underlying storage as
	// s, for stores that persist. It is nil for a store that does not.
	Reopen func(t *testing.T, s agentsession.Store) agentsession.Store
}

// Run exercises a store through the whole interface.
func Run(t *testing.T, opts Options) {
	t.Helper()
	if opts.New == nil {
		t.Fatal("storetest: Options.New is required")
	}
	t.Run("CreateAndOpen", func(t *testing.T) { testCreateAndOpen(t, opts) })
	t.Run("Append", func(t *testing.T) { testAppend(t, opts) })
	t.Run("List", func(t *testing.T) { testList(t, opts) })
	t.Run("Continue", func(t *testing.T) { testContinue(t, opts) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, opts) })
	if opts.Reopen != nil {
		t.Run("Persistence", func(t *testing.T) { testPersistence(t, opts) })
		t.Run("DurableLeaf", func(t *testing.T) { testDurableLeaf(t, opts) })
	}
}

// testDurableLeaf marks a branch, writes on it, and reopens the store.
// The mark names the branch that is live, not a fixed entry, so the
// reopened session resumes where the branch was written to. A store that
// rewinds to the mark loses every entry appended after it from the
// context and from every path-derived query.
func testDurableLeaf(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	first, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(first); err != nil {
		t.Fatal(err)
	}
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, mark); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("c"))); err != nil {
		t.Fatal(err)
	}
	last, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.AssistantText("d")))
	if err != nil {
		t.Fatal(err)
	}

	st2 := opts.Reopen(t, st)
	again, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after reopen: %v", err)
	}
	if again.Leaf() != last {
		t.Errorf("leaf after reopen = %s, want %s (the mark is %s)", again.Leaf(), last, first)
	}
	c, err := again.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 3 {
		t.Errorf("context after reopen = %d items, want 3", len(c.Items))
	}
}

func testCreateAndOpen(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{CWD: "/p", Harness: &agentsession.Harness{Name: "test"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := s.Header()
	if h.ID == "" || h.Format != agentsession.Format || h.Payload != agentsession.Payload || h.CreatedAt.IsZero() {
		t.Errorf("Create did not fill the header: %+v", h)
	}
	opened, err := st.Open(ctx, h.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !sameHeader(opened.Header(), h) {
		t.Errorf("Open header = %+v, want %+v", opened.Header(), h)
	}
	if _, err := st.Open(ctx, "missing"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open(missing) = %v, want ErrNoSession", err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: h.ID}); !errors.Is(err, agentsession.ErrSessionExists) {
		t.Errorf("Create(existing) = %v, want ErrSessionExists", err)
	}
	// A supplied ID is kept.
	s2, err := st.Create(ctx, agentsession.Header{ID: "given-id"})
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID() != "given-id" {
		t.Errorf("ID = %q", s2.ID())
	}
}

func testAppend(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	cfgID, err := st.Append(ctx, id, &agentsession.ConfigEntry{Model: "m"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if cfgID == "" {
		t.Fatal("Append returned no id")
	}
	userID, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	// The session handed out by Create sees the appends.
	if s.Len() != 2 || s.Leaf() != userID {
		t.Errorf("session after Append: len %d leaf %s", s.Len(), s.Leaf())
	}
	e, ok := s.Entry(userID)
	if !ok || e.Base().Parent != cfgID {
		t.Errorf("entry %s = %+v", userID, e)
	}
	// Leaf moves on the shared session steer later appends.
	if err := s.Branch(cfgID); err != nil {
		t.Fatal(err)
	}
	altID, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("other")))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Children(cfgID); len(got) != 2 || got[1] != altID {
		t.Errorf("children of config = %v", got)
	}
	s.ResetLeaf()
	rootID, err := st.Append(ctx, id, &agentsession.InfoEntry{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Roots(); len(got) != 2 || got[1] != rootID {
		t.Errorf("roots = %v", got)
	}
	// An extension entry goes through unchanged.
	raw := `{"type":"acme:note","text":"x"}`
	extID, err := st.Append(ctx, id, &agentsession.UnknownEntry{Raw: []byte(raw)})
	if err != nil {
		t.Fatalf("Append unknown: %v", err)
	}
	if e, ok := s.Entry(extID); !ok || e.EntryType() != "acme:note" {
		t.Errorf("unknown entry = %+v", e)
	}
	// Errors are reported.
	if _, err := st.Append(ctx, "missing", &agentsession.InfoEntry{}); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Append(missing) = %v", err)
	}
	if _, err := st.Append(ctx, id, &agentsession.InfoEntry{EntryBase: agentsession.EntryBase{ID: cfgID}}); !errors.Is(err, agentsession.ErrDuplicateEntry) {
		t.Errorf("Append(duplicate) = %v", err)
	}
	if _, err := st.Append(ctx, id, &agentsession.InfoEntry{EntryBase: agentsession.EntryBase{Parent: "ghost"}}); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("Append(bad parent) = %v", err)
	}
	if _, err := st.Append(ctx, id, &agentsession.ItemEntry{}); err == nil {
		t.Error("Append(item without item) succeeded")
	}
	// A failed append leaves the tree untouched.
	if s.Len() != 5 {
		t.Errorf("len after failed appends = %d, want 5", s.Len())
	}
	// Context builds from the store's session.
	if err := s.Branch(userID); err != nil {
		t.Fatal(err)
	}
	c, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Model != "m" || len(c.Items) != 1 {
		t.Errorf("context = %+v", c)
	}
}

func testList(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for i, cwd := range []string{"/a", "/b", "/a"} {
		h := agentsession.Header{ID: []string{"one", "two", "three"}[i], CWD: cwd, CreatedAt: base.Add(time.Duration(i) * time.Hour)}
		if i == 2 {
			h.ParentSession = "one"
		}
		if _, err := st.Create(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	// Names come from info entries; the last one that set a name wins.
	for _, name := range []string{"first name", "", "Two"} {
		if _, err := st.Append(ctx, "two", &agentsession.InfoEntry{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	collect := func(f agentsession.ListFilter) []string {
		var ids []string
		for sum, err := range st.List(ctx, f) {
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			ids = append(ids, sum.Header.ID)
			if f.WithNames {
				want := map[string]string{"two": "Two"}[sum.Header.ID]
				if sum.Name != want {
					t.Errorf("List name of %s = %q, want %q", sum.Header.ID, sum.Name, want)
				}
			}
		}
		return ids
	}
	tests := []struct {
		name string
		f    agentsession.ListFilter
		want []string
	}{
		{"all newest first", agentsession.ListFilter{}, []string{"three", "two", "one"}},
		{"cwd", agentsession.ListFilter{CWD: "/a"}, []string{"three", "one"}},
		{"parent", agentsession.ListFilter{ParentSession: "one"}, []string{"three"}},
		{"after", agentsession.ListFilter{After: base}, []string{"three", "two"}},
		{"before", agentsession.ListFilter{Before: base.Add(2 * time.Hour)}, []string{"two", "one"}},
		{"limit", agentsession.ListFilter{Limit: 2}, []string{"three", "two"}},
		{"with names", agentsession.ListFilter{WithNames: true}, []string{"three", "two", "one"}},
		{"with names filtered", agentsession.ListFilter{WithNames: true, CWD: "/b"}, []string{"two"}},
		{"none", agentsession.ListFilter{CWD: "/z"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collect(tt.f)
			if len(got) != len(tt.want) {
				t.Fatalf("List = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("List = %v, want %v", got, tt.want)
				}
			}
		})
	}
	// Stopping early is allowed.
	for range st.List(ctx, agentsession.ListFilter{}) {
		break
	}
}

func testDelete(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), &agentsession.InfoEntry{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Open(ctx, s.ID()); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open after Delete = %v", err)
	}
	if err := st.Delete(ctx, s.ID()); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("second Delete = %v", err)
	}
	for range st.List(ctx, agentsession.ListFilter{}) {
		t.Error("deleted session still listed")
	}
	// The ID can be reused.
	if _, err := st.Create(ctx, agentsession.Header{ID: s.ID()}); err != nil {
		t.Errorf("Create after Delete: %v", err)
	}
}

func testPersistence(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{CWD: "/p", Harness: &agentsession.Harness{Name: "h", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	cfgID, err := st.Append(ctx, id, &agentsession.ConfigEntry{Model: "m", Instructions: ptr("i")})
	if err != nil {
		t.Fatal(err)
	}
	userID, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, &agentsession.UnknownEntry{Raw: []byte(`{"type":"acme:note","text":"<x>","z":[1,2]}`)}); err != nil {
		t.Fatal(err)
	}
	asst := &agentsession.ItemEntry{Item: openresponses.AssistantText("yo"), ResponseID: "resp_1"}
	if _, err := st.Append(ctx, id, asst); err != nil {
		t.Fatal(err)
	}
	respID, err := st.Append(ctx, id, &agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	if err != nil {
		t.Fatal(err)
	}

	st2 := opts.Reopen(t, st)
	again, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after reopen: %v", err)
	}
	if !sameHeader(again.Header(), s.Header()) {
		t.Errorf("header after reopen = %+v, want %+v", again.Header(), s.Header())
	}
	if again.Len() != 5 || again.Leaf() != respID {
		t.Errorf("after reopen: len %d leaf %s, want 5 %s", again.Len(), again.Leaf(), respID)
	}
	c, err := again.ContextAt(userID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Model != "m" || c.Settings.Instructions != "i" || len(c.Items) != 1 || len(c.Entries) != 2 || c.Entries[0].Base().ID != cfgID {
		t.Errorf("context after reopen = %+v", c)
	}
	e, _ := again.Entry(respID)
	if r, ok := e.(*agentsession.ResponseEntry); !ok || r.Usage == nil || r.Usage.TotalTokens != 2 {
		t.Errorf("response after reopen = %+v", e)
	}
	// Appending through the second handle continues the same tree.
	moreID, err := st2.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("more")))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := again.Entry(moreID); e.Base().Parent != respID {
		t.Errorf("append after reopen parent = %s, want %s", e.Base().Parent, respID)
	}
	found := false
	for sum, err := range st2.List(ctx, agentsession.ListFilter{CWD: "/p"}) {
		if err != nil {
			t.Fatal(err)
		}
		if sum.Header.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("session not listed after reopen")
	}
	if err := st2.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := opts.Reopen(t, st2).Open(ctx, id); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open after Delete and reopen = %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// sameHeader compares headers field by field, with CreatedAt compared
// as an instant so a store that round-trips through RFC 3339 passes.
func sameHeader(a, b agentsession.Header) bool {
	return a.Format == b.Format && a.ID == b.ID && a.Payload == b.Payload &&
		a.CWD == b.CWD && a.ParentSession == b.ParentSession && a.Media == b.Media &&
		a.CreatedAt.Equal(b.CreatedAt) && reflect.DeepEqual(a.Harness, b.Harness) &&
		reflect.DeepEqual(a.Extra, b.Extra)
}

// testContinue rolls a session over and checks that the successor
// starts from the old settings, the old session is marked superseded,
// and a Current listing shows only the successor.
func testContinue(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	if _, err := st.Create(ctx, agentsession.Header{ID: "old", CWD: "/p"}); err != nil {
		t.Fatal(err)
	}
	instr := "Be brief."
	for _, e := range []agentsession.Entry{
		&agentsession.ConfigEntry{Model: "gpt-5", Instructions: &instr},
		&agentsession.InfoEntry{Name: "main"},
		agentsession.NewItemEntry(openresponses.UserText("hello")),
	} {
		if _, err := st.Append(ctx, "old", e); err != nil {
			t.Fatal(err)
		}
	}
	next, err := agentsession.Continue(ctx, st, "old", openresponses.DeveloperText("Earlier: the user said hello."))
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	nh := next.Header()
	if nh.ParentSession != "old" || nh.CWD != "/p" || next.Name() != "main" {
		t.Errorf("successor header = %+v name %q", nh, next.Name())
	}
	cx, err := next.Context()
	if err != nil {
		t.Fatal(err)
	}
	if cx.Settings.Model != "gpt-5" || cx.Settings.Instructions != "Be brief." || len(cx.Items) != 1 {
		t.Errorf("successor context = %+v, %d items", cx.Settings, len(cx.Items))
	}
	if _, ok := next.Entries()[0].(*agentsession.ConfigEntry); !ok {
		t.Error("successor's first entry is not a config")
	}
	old, err := st.Open(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if old.SupersededBy() != next.ID() {
		t.Errorf("old.SupersededBy = %q, want %s", old.SupersededBy(), next.ID())
	}
	ids := func(f agentsession.ListFilter) map[string]agentsession.Summary {
		out := map[string]agentsession.Summary{}
		for sum, err := range st.List(ctx, f) {
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			out[sum.Header.ID] = sum
		}
		return out
	}
	all := ids(agentsession.ListFilter{WithNames: true})
	if len(all) != 2 || all["old"].SupersededBy != next.ID() || all[next.ID()].SupersededBy != "" {
		t.Errorf("listing = %+v", all)
	}
	current := ids(agentsession.ListFilter{Current: true})
	if len(current) != 1 || current[next.ID()].Header.ID == "" {
		t.Errorf("current listing = %+v, want only %s", current, next.ID())
	}
}
