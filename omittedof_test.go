package agentsession

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// omitList is n omitted parts named p0, p1, ... under one reason, so a
// test can say which of them a delta kept.
func omitList(prefix string, n int) []OmittedPart {
	var out []OmittedPart
	for i := 0; i < n; i++ {
		out = append(out, OmittedPart{ID: fmt.Sprintf("%s%d", prefix, i), Reason: "budget", Size: 10 + i, Source: "agentmemory"})
	}
	return out
}

// omitIDs lists the ids of a resolved list, an unresolved element as
// "keep:n" or "keep:n@of".
func omitIDs(list []OmittedPart) string {
	var ids []string
	for _, o := range list {
		switch {
		case o.ID != "":
			ids = append(ids, o.ID)
		case o.Of != "":
			ids = append(ids, fmt.Sprintf("keep:%d@%s", o.Keep, o.Of))
		case o.Keep > 0:
			ids = append(ids, fmt.Sprintf("keep:%d", o.Keep))
		default:
			ids = append(ids, "?")
		}
	}
	return strings.Join(ids, ",")
}

// omitSession appends each config, in order, to a session and returns
// the session with the ID each was appended as; a nil entry is a user
// message, so a test can put something between two configs.
func omitSession(t *testing.T, entries ...Entry) (*Session, []string) {
	t.Helper()
	s := New(Header{})
	ids := make([]string, len(entries))
	for i, e := range entries {
		if e == nil {
			e = NewItemEntry(openresponses.UserText("between"))
		}
		id, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		ids[i] = id
	}
	return s, ids
}

// TestOmittedKeepOf: a keep carrying of counts over the list the entry
// it names put in force, as resolved at that entry; each list has a
// cursor of its own, a keep moves its own list's, and an element naming
// a part moves the cursor of every list that names it (#173).
func TestOmittedKeepOf(t *testing.T) {
	long := omitList("p", 6) // p0..p5
	other := omitList("q", 2)
	for _, tt := range []struct {
		name string
		// delta is the list the last config carries, given the id the
		// first config was appended as.
		delta func(first string) []OmittedPart
		want  string
	}{
		{"the whole list", func(a string) []OmittedPart { return []OmittedPart{{Keep: 6, Of: a}} }, "p0,p1,p2,p3,p4,p5"},
		{"a run from the start", func(a string) []OmittedPart { return []OmittedPart{{Keep: 2, Of: a}} }, "p0,p1"},
		{"a new part, then the list", func(a string) []OmittedPart {
			return []OmittedPart{{ID: "new", Reason: "budget"}, {Keep: 6, Of: a}}
		}, "new,p0,p1,p2,p3,p4,p5"},
		{"a keep continues its own cursor", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 2, Of: a}, {ID: "new", Reason: "budget"}, {Keep: 4, Of: a}}
		}, "p0,p1,new,p2,p3,p4,p5"},
		{"a changed part skips its old self in the earlier list", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 2, Of: a}, {ID: "p2", Reason: "moved", Size: 1}, {Keep: 3, Of: a}}
		}, "p0,p1,p2,p3,p4,p5"},
		{"a keep without of counts over the list in force", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 1}, {Keep: 1, Of: a}}
		}, "q0,p0"},
		{"of moves its own cursor only", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 3, Of: a}, {Keep: 1}}
		}, "p0,p1,p2,q0"},
		{"of beside an id means nothing", func(a string) []OmittedPart {
			return []OmittedPart{{ID: "p1", Reason: "x", Of: a}}
		}, "p1"},
		{"an entry that names no list", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 2, Of: "sha256:nowhere"}, {Keep: 1}}
		}, "keep:2@sha256:nowhere,q0"},
		{"a keep past the list it names", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 7, Of: a}}
		}, "keep:7@"},
		{"a keep over a part the delta names", func(a string) []OmittedPart {
			return []OmittedPart{{Keep: 3, Of: a}, {ID: "p1", Reason: "x"}}
		}, "keep:3@,p1"},
		{"an of with no keep is kept as written", func(a string) []OmittedPart {
			return []OmittedPart{{Of: a}}
		}, "keep:0@"}, // names no part: kept as written, of included
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The first config's list is long, the second's other, and
			// the last config carries the delta over both. A file is
			// read rather than appended to, since a writer may not write
			// an of beside an id, which a reader still reads.
			bodies := func(delta string) []string {
				return []string{
					`"type":"config","model":"m","instructions_omitted":` + string(mustJSON(t, long)),
					`"type":"config","instructions_omitted":` + string(mustJSON(t, other)),
					`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"between"}]}`,
					`"type":"config","instructions_omitted":` + delta,
				}
			}
			_, ids := hashedLines(t, Format, bodies("[]")...)
			in, _ := hashedLines(t, Format, bodies(string(mustJSON(t, tt.delta(ids[0]))))...)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := s.Context()
			if err != nil {
				t.Fatal(err)
			}
			got := omitIDs(ctx.Settings.InstructionsOmitted)
			// An unresolved of is printed with the entry it named, which
			// the table spells as nothing where it is the first config.
			got = strings.ReplaceAll(got, ids[0], "")
			if got != tt.want {
				t.Errorf("list in force %s, want %s", got, tt.want)
			}
		})
	}
}

// TestOmittedOfRecursion: the list an entry put in force is the list
// resolved at that entry, so an of may name an entry whose own list was
// written by of.
func TestOmittedOfRecursion(t *testing.T) {
	s, ids := omitSession(t, &ConfigEntry{InstructionsOmitted: omitList("p", 4)})
	second := []OmittedPart{{ID: "new", Reason: "budget"}}
	// The second entry's list is a part and the first's whole list.
	second = append(second, OmittedPart{Keep: 4, Of: ids[0]})
	id2, err := s.Append(&ConfigEntry{InstructionsOmitted: second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{ID: "other", Reason: "budget"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 5, Of: id2}}}); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "new,p0,p1,p2,p3" {
		t.Errorf("list in force %s", got)
	}
}

// TestOmittedOfLeavesForce: a replace and a compaction's checkpoint
// start the lists afresh, so an of after one that names an entry before
// it is unresolved; an entry the replace itself wrote can be named; and
// an of that names an entry after, or the entry itself, names nothing.
func TestOmittedOfLeavesForce(t *testing.T) {
	list := omitList("p", 3)
	t.Run("a replace", func(t *testing.T) {
		s, ids := omitSession(t,
			&ConfigEntry{Model: "m", InstructionsOmitted: list},
			&ConfigEntry{Replace: true, Model: "m", InstructionsOmitted: omitList("r", 2)},
		)
		if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3, Of: ids[0]}}}); err != nil {
			t.Fatal(err)
		}
		ctx, _ := s.Context()
		if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "keep:3@"+ids[0] {
			t.Errorf("an of before a replace: %s", got)
		}
		if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 2, Of: ids[1]}}}); err != nil {
			t.Fatal(err)
		}
		ctx, _ = s.Context()
		if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "r0,r1" {
			t.Errorf("an of naming the replace's own list: %s", got)
		}
	})
	t.Run("a replace without the member", func(t *testing.T) {
		s, ids := omitSession(t,
			&ConfigEntry{Model: "m", InstructionsOmitted: list},
			&ConfigEntry{Replace: true, Model: "m"},
		)
		if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3, Of: ids[0]}}}); err != nil {
			t.Fatal(err)
		}
		ctx, _ := s.Context()
		if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "keep:3@"+ids[0] {
			t.Errorf("an of before a replace that wrote none: %s", got)
		}
	})
	t.Run("a compaction", func(t *testing.T) {
		s, ids := omitSession(t,
			&ConfigEntry{Model: "m", InstructionsOmitted: list},
			nil,
		)
		comp, err := s.CompactKeeping(1, openresponses.UserText("summary"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(comp); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3, Of: ids[0]}}}); err != nil {
			t.Fatal(err)
		}
		ctx, _ := s.Context()
		if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "keep:3@"+ids[0] {
			t.Errorf("an of before a checkpoint: %s", got)
		}
		// The checkpoint carries the list whole, and is not a config
		// entry, so a delta after it counts over it as the list in
		// force.
		if got := len(comp.Config.InstructionsOmitted); got != 3 {
			t.Fatalf("the checkpoint carries %d parts", got)
		}
	})
	t.Run("an entry that carries no list", func(t *testing.T) {
		s, ids := omitSession(t, &ConfigEntry{Model: "m", InstructionsOmitted: list}, &ConfigEntry{Model: "n"})
		if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3, Of: ids[1]}}}); err != nil {
			t.Fatal(err)
		}
		ctx, _ := s.Context()
		if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "keep:3@"+ids[1] {
			t.Errorf("an of naming an entry with no list: %s", got)
		}
	})
	t.Run("an entry on another branch", func(t *testing.T) {
		s, ids := omitSession(t, &ConfigEntry{Model: "m"}, &ConfigEntry{InstructionsOmitted: list})
		if err := s.Branch(ids[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3, Of: ids[1]}}}); err != nil {
			t.Fatal(err)
		}
		ctx, _ := s.Context()
		if got := omitIDs(ctx.Settings.InstructionsOmitted); got != "keep:3@"+ids[1] {
			t.Errorf("an of naming an entry off the path: %s", got)
		}
	})
}

// TestOmittedOfRoundTrip: of is read and written as a member of a keep,
// and an of that is not a non-empty string is kept as the file wrote it.
func TestOmittedOfRoundTrip(t *testing.T) {
	line := `{"type":"config","id":"c","parent":null,"ts":"2026-10-01T12:00:00Z","instructions_omitted":[{"keep":3,"of":"sha256:abc"},{"id":"p","reason":"budget","size":4}]}`
	e, err := UnmarshalEntry([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	list := e.(*ConfigEntry).InstructionsOmitted
	if len(list) != 2 || list[0].Keep != 3 || list[0].Of != "sha256:abc" {
		t.Fatalf("decoded %+v", list)
	}
	back, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, back, []byte(line)) {
		t.Errorf("rewritten as %s", back)
	}
	for _, of := range []string{`7`, `["x"]`, `""`, `null`} {
		odd := strings.Replace(line, `"sha256:abc"`, of, 1)
		e, err := UnmarshalEntry([]byte(odd))
		if err != nil {
			t.Fatalf("of %s: %v", of, err)
		}
		if got := e.(*ConfigEntry).InstructionsOmitted[0]; got.Of != "" || got.Keep != 3 {
			t.Errorf("of %s decoded as %+v", of, got)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, back, []byte(odd)) {
			t.Errorf("of %s rewritten as %s", of, back)
		}
	}
}

// TestOmittedOfValidation: a writer's of is a member of a keep and of
// nothing else, and a replace carries none.
func TestOmittedOfValidation(t *testing.T) {
	s := New(Header{})
	for name, c := range map[string]*ConfigEntry{
		"of beside an id":      {InstructionsOmitted: []OmittedPart{{ID: "a", Of: "x"}}},
		"of beside members":    {InstructionsOmitted: []OmittedPart{{Keep: 1, Size: 3, Of: "x"}}},
		"of with no keep":      {InstructionsOmitted: []OmittedPart{{Of: "x"}}},
		"of in a replace":      {Replace: true, InstructionsOmitted: []OmittedPart{{Keep: 1, Of: "x"}}},
		"a part named twice":   {InstructionsOmitted: []OmittedPart{{ID: "a"}, {ID: "a"}, {Keep: 1, Of: "x"}}},
		"a keep and its of ok": nil,
	} {
		if c == nil {
			if _, err := s.Append(&ConfigEntry{InstructionsOmitted: []OmittedPart{{ID: "a", Reason: "r"}, {Keep: 2, Of: "x"}}}); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if _, err := s.Append(c); err == nil {
			t.Errorf("%s: appended", name)
		}
	}
}

// TestOmittedDeltaOf: OmittedDelta names the list an earlier entry
// put in force when that is shorter than the whole list and the keeps
// over the list in force, and never when it is not.
func TestOmittedDeltaOf(t *testing.T) {
	a := omitList("a", 40)
	b := omitList("b", 40)
	build := func(t *testing.T) (*Session, []string) {
		s, ids := omitSession(t,
			&ConfigEntry{Model: "m", InstructionsOmitted: a},
			&ConfigEntry{InstructionsOmitted: b},
		)
		return s, ids
	}
	t.Run("a hand-back names the earlier list", func(t *testing.T) {
		s, ids := build(t)
		ctx, _ := s.Context()
		d := ctx.Settings.OmittedDelta(a)
		if len(d) != 1 || d[0].Keep != 40 || d[0].Of != ids[0] || d[0].ID != "" {
			t.Fatalf("delta %+v", d)
		}
		// And it resolves to the list.
		if got := ctx.Settings.Apply(&ConfigEntry{InstructionsOmitted: d}).InstructionsOmitted; omitIDs(got) != omitIDs(a) {
			t.Errorf("resolved to %s", omitIDs(got))
		}
	})
	t.Run("a change in the middle keeps both halves", func(t *testing.T) {
		s, ids := build(t)
		ctx, _ := s.Context()
		next := append(append(append([]OmittedPart(nil), a[:10]...), OmittedPart{ID: "new", Reason: "budget"}), a[10:]...)
		d := ctx.Settings.OmittedDelta(next)
		if omitIDs(d) != fmt.Sprintf("keep:10@%s,new,keep:30@%s", ids[0], ids[0]) {
			t.Fatalf("delta %s", omitIDs(d))
		}
		if got := ctx.Settings.Apply(&ConfigEntry{InstructionsOmitted: d}).InstructionsOmitted; omitIDs(got) != omitIDs(next) {
			t.Errorf("resolved to %s", omitIDs(got))
		}
	})
	t.Run("a part moved out of order is written whole", func(t *testing.T) {
		s, _ := build(t)
		ctx, _ := s.Context()
		next := append([]OmittedPart{a[5]}, append(append([]OmittedPart(nil), a[:5]...), a[6:]...)...)
		d := ctx.Settings.OmittedDelta(next)
		if got := ctx.Settings.Apply(&ConfigEntry{InstructionsOmitted: d}).InstructionsOmitted; omitIDs(got) != omitIDs(next) {
			t.Errorf("resolved to %s from %s", omitIDs(got), omitIDs(d))
		}
	})
	t.Run("the list in force is not named by of", func(t *testing.T) {
		s, _ := build(t)
		ctx, _ := s.Context()
		next := append(append([]OmittedPart(nil), b[:20]...), OmittedPart{ID: "new", Reason: "budget"})
		next = append(next, b[20:]...)
		for _, o := range ctx.Settings.OmittedDelta(next) {
			if o.Of != "" {
				t.Errorf("the keeps over the list in force were replaced by %+v", o)
			}
		}
	})
	t.Run("a short list is written whole", func(t *testing.T) {
		s, _ := omitSession(t,
			&ConfigEntry{Model: "m", InstructionsOmitted: []OmittedPart{{ID: "a", Reason: "r"}}},
			&ConfigEntry{InstructionsOmitted: []OmittedPart{{ID: "b", Reason: "r"}}},
		)
		ctx, _ := s.Context()
		d := ctx.Settings.OmittedDelta([]OmittedPart{{ID: "a", Reason: "r"}})
		for _, o := range d {
			if o.Of != "" {
				t.Errorf("a one-element list names an entry: %+v", d)
			}
		}
	})
	t.Run("no history, no of", func(t *testing.T) {
		settings := Settings{InstructionsOmitted: b}
		for _, o := range settings.OmittedDelta(a) {
			if o.Of != "" {
				t.Errorf("settings built by hand name an entry: %+v", o)
			}
		}
	})
	t.Run("a replace forgets the lists", func(t *testing.T) {
		s, ids := omitSession(t,
			&ConfigEntry{Model: "m", InstructionsOmitted: a},
			&ConfigEntry{Replace: true, Model: "m", InstructionsOmitted: b},
		)
		ctx, _ := s.Context()
		for _, o := range ctx.Settings.OmittedDelta(a) {
			if o.Of == ids[0] {
				t.Errorf("a list before the replace is named: %+v", o)
			}
		}
	})
	t.Run("only the most recent lists are tried", func(t *testing.T) {
		entries := []Entry{&ConfigEntry{Model: "m", InstructionsOmitted: a}}
		for i := 0; i < maxOmittedLists+2; i++ {
			entries = append(entries, &ConfigEntry{InstructionsOmitted: omitList(fmt.Sprintf("x%d_", i), 3)})
		}
		s, ids := omitSession(t, entries...)
		ctx, _ := s.Context()
		for _, o := range ctx.Settings.OmittedDelta(a) {
			if o.Of == ids[0] {
				t.Errorf("a list further back than %d is named: %+v", maxOmittedLists, o)
			}
		}
	})
}

// TestHandbackFixture reads the 0.11 conformance fixture: an agent
// handed the session back names every part that left force by hash and
// its omitted list by of, every response's request hash verifies, the
// lists in force are the ones written, and an of that names no entry on
// the path is kept as written beside a request that still verifies.
func TestHandbackFixture(t *testing.T) {
	for name, bad := range map[string]bool{"handback": false, "bad-handback": true} {
		raw, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		if err := Write(&buf, handbackFixture(t, bad)); err != nil {
			t.Fatal(err)
		}
		if buf.String() != string(raw) {
			t.Errorf("%s.jsonl is not what handbackFixture builds; run go test -update", name)
		}
		s := loadFixture(t, name)
		var configs []*ConfigEntry
		responses := 0
		for _, e := range s.Entries() {
			switch v := e.(type) {
			case *ConfigEntry:
				configs = append(configs, v)
			case *ResponseEntry:
				responses++
				if err := s.Verify(v.ID); err != nil {
					t.Errorf("%s: %s: %v", name, v.ResponseID, err)
				}
			}
		}
		if len(configs) != 5 || responses != 5 {
			t.Fatalf("%s: %d configs, %d responses, want 5 each", name, len(configs), responses)
		}
		// Every part a hand-back names is by hash, bar the new fact.
		for _, i := range []int{2, 3, 4} {
			for _, p := range configs[i].InstructionsParts {
				if (p.Text != "") != (p.ID == "memory/user/n-0020" && i == 2) {
					t.Errorf("%s: config %d part %s: text %v", name, i+1, p.ID, p.Text != "")
				}
			}
		}
		at := func(i int) string {
			ctx, err := s.ContextAt(configs[i].ID)
			if err != nil {
				t.Fatal(err)
			}
			return omitIDs(ctx.InstructionsOmitted())
		}
		ids := func(from, to int) string {
			var out []string
			for n := from; n <= to; n++ {
				out = append(out, fmt.Sprintf("memory/user/n-%04d", n))
			}
			return strings.Join(out, ",")
		}
		wantA := ids(4, 19)
		wantA2 := ids(3, 19)
		if got := at(0); got != wantA {
			t.Errorf("%s: list at 1 is %s", name, got)
		}
		if got := at(1); got != "skills/dispute,skills/audit" {
			t.Errorf("%s: list at 2 is %s", name, got)
		}
		if got := at(2); got != wantA2 {
			t.Errorf("%s: list at 3 is %s, want %s", name, got, wantA2)
		}
		if got := at(3); got != "skills/dispute,skills/audit" {
			t.Errorf("%s: list at 4 is %s", name, got)
		}
		if bad {
			if got := at(4); !strings.HasPrefix(got, "keep:17@sha256:") {
				t.Errorf("%s: list at 5 is %s, want the unresolved keep", name, got)
			}
			continue
		}
		if got := at(4); got != wantA2 {
			t.Errorf("%s: list at 5 is %s, want %s", name, got, wantA2)
		}
		// The hand-back is a keep of a list an earlier entry wrote.
		if l := configs[2].InstructionsOmitted; len(l) != 2 || l[1].Keep != 16 || l[1].Of != configs[0].ID {
			t.Errorf("config 3 omitted %+v", l)
		}
		if l := configs[4].InstructionsOmitted; len(l) != 1 || l[0].Keep != 17 || l[0].Of != configs[2].ID {
			t.Errorf("config 5 omitted %+v", l)
		}
		// And it costs a fraction of what the first config wrote.
		sizeOf := func(c *ConfigEntry) int {
			line, err := MarshalEntry(c)
			if err != nil {
				t.Fatal(err)
			}
			return len(line)
		}
		if first, back := sizeOf(configs[0]), sizeOf(configs[4]); back*2 > first {
			t.Errorf("the hand-back is %d bytes beside the first config's %d", back, first)
		}
	}
}
