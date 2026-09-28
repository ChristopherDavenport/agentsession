package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// TestMigrationFromV04 reads every 0.4 source, checks that the migration
// gave each entry a hash for an id and kept the old one in legacy_id,
// and that the migrated tree is the one the generated fixture holds.
func TestMigrationFromV04(t *testing.T) {
	for _, name := range fixtureNames {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", "sessions", "v0.4", name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			m, err := Read(f)
			if err != nil {
				t.Fatal(err)
			}
			migrated, unresolved := m.Migrated()
			if !migrated {
				t.Fatal("a 0.4 file did not report as migrated")
			}
			if m.Header().Format != Format {
				t.Errorf("format = %s, want %s", m.Header().Format, Format)
			}
			gen := loadFixture(t, name)
			if m.Len() != gen.Len() {
				t.Fatalf("migrated %d entries, fixture has %d", m.Len(), gen.Len())
			}
			for i, e := range m.Entries() {
				b := e.Base()
				if !strings.HasPrefix(b.ID, HashPrefix) || b.LegacyID == "" {
					t.Errorf("entry %d: id %s legacy %q", i, b.ID, b.LegacyID)
				}
				if got := gen.Entries()[i].Base().ID; got != b.ID {
					t.Errorf("entry %s: migrated id %s, fixture %s", b.LegacyID, b.ID, got)
				}
				if _, ok := ParseCanonicalTime(CanonicalTime(b.Timestamp)); !ok {
					t.Errorf("entry %s: ts not canonical", b.LegacyID)
				}
			}
			if m.Leaf() != gen.Leaf() {
				t.Errorf("leaf %s, fixture %s", m.Leaf(), gen.Leaf())
			}
			// A migrated file with an extension entry cannot be re-emitted,
			// since the reader cannot rewrite what such an entry names.
			var buf bytes.Buffer
			err = Write(&buf, m)
			if len(unresolved) > 0 && !errors.Is(err, ErrUnresolvedMigration) {
				t.Errorf("Write = %v with %d unresolved", err, len(unresolved))
			}
			if len(unresolved) == 0 && err != nil {
				t.Errorf("Write = %v", err)
			}
		})
	}
}

// TestTwoLayerHash checks the shape of the two hashes: the id is over the
// envelope and the content hash, so a change to ts moves the id and not
// the content hash, and a change to the body moves both.
func TestTwoLayerHash(t *testing.T) {
	mk := func(text string, ts time.Time) *ItemEntry {
		e := NewItemEntry(openresponses.UserText(text))
		e.Timestamp = ts
		return e
	}
	t0 := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	a := mk("hello", t0)
	b := mk("hello", t0.Add(time.Second))
	c := mk("other", t0)
	for _, e := range []*ItemEntry{a, b, c} {
		s := New(Header{})
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if a.ContentHash() != b.ContentHash() || a.ID == b.ID {
		t.Errorf("ts: content %s/%s id %s/%s", a.ContentHash(), b.ContentHash(), a.ID, b.ID)
	}
	if a.ContentHash() == c.ContentHash() || a.ID == c.ID {
		t.Error("a different body shares a hash")
	}
	// The id is the hash of the envelope object as the format defines it,
	// computed independently here.
	env := `{"content":"` + a.ContentHash() + `","parent":null,"ts":"2026-09-17T12:00:00Z","type":"item"}`
	want, err := HashRequestJSON([]byte(env))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != want {
		t.Errorf("id = %s, want %s from the envelope", a.ID, want)
	}
	// And EntryHashes agrees with what Append set.
	data, _ := MarshalEntry(a)
	id, content, err := EntryHashes(data)
	if err != nil || id != a.ID || content != a.ContentHash() {
		t.Errorf("EntryHashes = %s %s %v", id, content, err)
	}
}

// TestCanonicalTS checks the one spelling the format admits, that a
// caller's zoned time is taken to UTC, and that a 0.5 line spelled
// otherwise is refused.
func TestCanonicalTS(t *testing.T) {
	for s, ok := range map[string]bool{
		"2026-09-17T12:00:02Z":           true,
		"2026-09-17T12:00:02.5Z":         true,
		"2026-09-17T12:00:02.123456789Z": true,
		"2026-09-17T12:00:02.500Z":       false,
		"2026-09-17T12:00:02+00:00":      false,
		"2026-09-17t12:00:02Z":           false,
		"2026-09-17T13:00:02+01:00":      false,
	} {
		if _, got := ParseCanonicalTime(s); got != ok {
			t.Errorf("ParseCanonicalTime(%s) = %v, want %v", s, got, ok)
		}
	}
	loc := time.FixedZone("x", 3600)
	e := NewItemEntry(openresponses.UserText("z"))
	e.Timestamp = time.Date(2026, 9, 17, 13, 0, 0, 500000000, loc)
	s := New(Header{})
	if _, err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalTime(e.Timestamp); got != "2026-09-17T12:00:00.5Z" || e.Timestamp.Location() != time.UTC {
		t.Errorf("ts = %s in %v", got, e.Timestamp.Location())
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	// Respell the ts and the line no longer reads, however it hashes.
	bad := strings.Replace(buf.String(), `"ts":"2026-09-17T12:00:00.5Z"`, `"ts":"2026-09-17T12:00:00.500Z"`, 1)
	if bad == buf.String() {
		t.Fatal("the fixture line was not found")
	}
	if _, err := Read(strings.NewReader(bad)); !errors.Is(err, ErrBadID) {
		t.Errorf("Read of a respelled ts = %v, want ErrBadID", err)
	}
}

// TestReservedAndIJSON checks that a body carrying an envelope name, or a
// member that is not I-JSON and cannot be normalised, is refused by
// Append and by Read.
func TestReservedAndIJSON(t *testing.T) {
	s := New(Header{})
	e := &InfoEntry{Name: "n"}
	e.Unknown = map[string]json.RawMessage{"content": json.RawMessage(`1`)}
	if _, err := s.Append(e); !errors.Is(err, ErrReservedMember) {
		t.Errorf("Append with a body content = %v", err)
	}
	// A whole number outside binary64 is normalised, not refused; see
	// TestAppendNormalises. What a rewrite cannot reach still is.
	repeated := &InfoEntry{Name: "n"}
	repeated.Unknown = map[string]json.RawMessage{"acme:seed": json.RawMessage(`{"a":1,"a":2}`)}
	if _, err := s.Append(repeated); err == nil || !strings.Contains(err.Error(), "not I-JSON") {
		t.Errorf("Append with a repeated member = %v", err)
	}
	huge := &InfoEntry{Name: "n"}
	huge.Unknown = map[string]json.RawMessage{"acme:seed": json.RawMessage(`1e400`)}
	if _, err := s.Append(huge); err == nil || !strings.Contains(err.Error(), "not finite") {
		t.Errorf("Append with 1e400 = %v", err)
	}
	ok := &InfoEntry{Name: "n"}
	ok.Unknown = map[string]json.RawMessage{"acme:seed": json.RawMessage(`9007199254740992`), "acme:f": json.RawMessage(`0.1`)}
	if _, err := s.Append(ok); err != nil {
		t.Errorf("Append with representable numbers = %v", err)
	}
	header := `{"type":"session","format":"agentsession/0.5","id":"x","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}` + "\n"
	line := `{"type":"info","id":"sha256:00","parent":null,"ts":"2026-09-17T16:00:00Z","name":"n","content":1}` + "\n"
	if _, err := Read(strings.NewReader(header + line)); !errors.Is(err, ErrReservedMember) {
		t.Errorf("Read of a body content = %v", err)
	}
}

// TestContextHash checks the cache key: it ignores ts and record
// entries, moves with the body, and is computable across a compaction.
func TestContextHash(t *testing.T) {
	s := loadFixture(t, "basic")
	leaf := s.Leaf()
	h, err := s.ContextHash(leaf)
	if err != nil || !strings.HasPrefix(h, HashPrefix) {
		t.Fatalf("ContextHash = %s, %v", h, err)
	}
	// Replay with every ts moved and a record entry added: same key.
	replay := New(Header{})
	ids := map[string]string{}
	for _, e := range s.Entries() {
		data, _ := MarshalEntry(e)
		c, err := UnmarshalEntry(data)
		if err != nil {
			t.Fatal(err)
		}
		b := c.Base()
		b.ID = ""
		b.LegacyID = ""
		b.Timestamp = b.Timestamp.Add(time.Hour)
		if b.Parent != "" {
			b.Parent = ids[b.Parent]
		}
		// The recorded targets name entries by id; rewrite them.
		if o, ok := c.(*OutcomeEntry); ok {
			o.Target = ids[o.Target]
		}
		if d, ok := c.(*DispatchEntry); ok {
			d.Target = ids[d.Target]
		}
		id, err := replay.Append(c)
		if err != nil {
			t.Fatal(err)
		}
		ids[e.Base().ID] = id
	}
	if _, err := replay.Append(&InfoEntry{Name: "noted"}); err != nil {
		t.Fatal(err)
	}
	h2, err := replay.ContextHash(replay.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if h != h2 {
		t.Errorf("context hash moved with ts, legacy_id or a record entry: %s vs %s", h, h2)
	}
	if _, err := replay.Append(NewItemEntry(openresponses.UserText("more"))); err != nil {
		t.Fatal(err)
	}
	if h3, _ := replay.ContextHash(replay.Leaf()); h3 == h {
		t.Error("context hash did not move with a new item")
	}
	comp := loadFixture(t, "compaction")
	if _, err := comp.ContextHash(comp.Leaf()); err != nil {
		t.Errorf("ContextHash across a compaction = %v", err)
	}
}

// TestForkSession checks a session with a base: it opens with the
// origin's path, its own entries hang from the base, branching above
// the base is refused, a leaf label may not name a prefix entry, and
// the file it writes reads back with the same prefix and leaf.
func TestForkSession(t *testing.T) {
	origin := loadFixture(t, "basic")
	at := lid(t, origin, "i0000004")
	f, err := Fork(origin, at, Header{ID: "01995b2a-0000-7000-8000-000000000010"})
	if err != nil {
		t.Fatal(err)
	}
	if h := f.Header(); h.Base != at || h.ParentSession != origin.ID() {
		t.Errorf("header = %+v", h)
	}
	if f.Leaf() != at || !f.Prefix(at) || f.Len() != len(origin.Path(at)) {
		t.Errorf("fork opens leaf %s len %d", f.Leaf(), f.Len())
	}
	// Above the base is another session's business.
	above := NewItemEntry(openresponses.UserText("x"))
	above.Parent = lid(t, origin, "i0000001")
	if _, err := f.Append(above); !errors.Is(err, ErrNoEntry) {
		t.Errorf("Append above the base = %v", err)
	}
	own, err := f.Append(NewItemEntry(openresponses.UserText("a different answer")))
	if err != nil {
		t.Fatal(err)
	}
	if f.Leaf() != own || f.Prefix(own) {
		t.Errorf("own entry: leaf %s prefix %v", f.Leaf(), f.Prefix(own))
	}
	// A leaf label naming a prefix entry moves nothing.
	if _, err := f.Append(NewLabelEntry(lid(t, origin, "i0000001"), LeafLabel)); err != nil {
		t.Fatal(err)
	}
	if f.Leaf() != own {
		t.Errorf("leaf moved onto the prefix: %s", f.Leaf())
	}
	var buf bytes.Buffer
	if err := Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	again, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if again.Leaf() != own || !again.Prefix(at) || again.Prefix(own) || again.Header().Base != at {
		t.Errorf("read back: leaf %s prefix(at) %v prefix(own) %v", again.Leaf(), again.Prefix(at), again.Prefix(own))
	}
	// The fork's first request is the origin's at that point: the shared
	// prefix a provider caches.
	if ho, hf := requestHashAt(t, origin, at), requestHashAt(t, again, at); ho != hf {
		t.Errorf("request at the base differs: %s vs %s", ho, hf)
	}
	if co, cf := mustCtx(t, origin, at), mustCtx(t, again, at); co != cf {
		t.Errorf("context hash at the base differs: %s vs %s", co, cf)
	}
	// A base may not be a leaf label.
	if _, err := origin.Append(NewLabelEntry(at, LeafLabel)); err != nil {
		t.Fatal(err)
	}
	labelID := origin.Entries()[origin.Len()-1].Base().ID
	if _, err := Fork(origin, labelID, Header{}); err == nil {
		t.Error("Fork at a leaf label succeeded")
	}
}

func mustCtx(t *testing.T, s *Session, id string) string {
	t.Helper()
	h, err := s.ContextHash(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestMigrationReportsUnresolved: a 0.4 reference the reader cannot
// rewrite — into another session, or to an id not seen earlier — is
// reported, and the file is not re-emitted as 0.5.
func TestMigrationReportsUnresolved(t *testing.T) {
	m := loadV04(t, "converge")
	migrated, unresolved := m.Migrated()
	if !migrated || len(unresolved) == 0 {
		t.Fatalf("converge carries references into other sessions; unresolved = %v", unresolved)
	}
	var buf bytes.Buffer
	if err := Write(&buf, m); !errors.Is(err, ErrUnresolvedMigration) {
		t.Errorf("Write of a migrated file with unresolved references = %v", err)
	}
	// A file whose references all resolve writes.
	if b := loadV04(t, "basic"); len(mustMigrated(t, b)) != 0 {
		t.Errorf("basic reported unresolved references: %v", mustMigrated(t, b))
	}
}

func loadV04(t *testing.T, name string) *Session {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "sessions", "v0.4", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustMigrated(t *testing.T, s *Session) []string {
	t.Helper()
	_, unresolved := s.Migrated()
	return unresolved
}

// TestEntryHashesParentsSpelling: an empty parents however spelled is
// omitted from the envelope, so two spellings give one id.
func TestEntryHashesParentsSpelling(t *testing.T) {
	base := `{"type":"info","id":"","parent":null,"ts":"2026-09-17T12:00:00Z","name":"n"}`
	want, _, err := EntryHashes([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, spelled := range []string{`[]`, `[ ]`, "[\n]", `null`} {
		line := strings.Replace(base, `"parent":null,`, `"parent":null,"parents":`+spelled+`,`, 1)
		got, _, err := EntryHashes([]byte(line))
		if err != nil {
			t.Fatalf("%s: %v", spelled, err)
		}
		if got != want {
			t.Errorf("parents %q gave id %s, want %s", spelled, got, want)
		}
	}
}

// TestContextHashVector pins the context hash to RFC 0002's definition
// with values computed here from the rules alone: the seed is the hash
// of canonical null; a contributing entry hashes the array of the
// parent's context hash, its type and its content hash; a record entry
// inherits; response, source and queued_from leave an item's body; a
// compaction's first_kept becomes [context hash, type] of the entry it
// names.
func TestContextHashVector(t *testing.T) {
	s := New(Header{})
	seed, _ := HashRequestJSON([]byte("null"))
	step := func(prev, typ, content string) string {
		arr, _ := json.Marshal([]string{prev, typ, content})
		h, _ := HashRequestJSON(arr)
		return h
	}
	bodyHash := func(members string) string {
		h, _ := HashRequestJSON([]byte(members))
		return h
	}
	cfg := &ConfigEntry{Model: "m"}
	cfgID, _ := s.Append(cfg)
	want := step(seed, TypeConfig, bodyHash(`{"model":"m"}`))
	if got, _ := s.ContextHash(cfgID); got != want {
		t.Errorf("config: %s, want %s", got, want)
	}
	// A record entry inherits.
	runID, _ := s.Append(NewRunStart("r1", SourceInput, ""))
	if got, _ := s.ContextHash(runID); got != want {
		t.Errorf("run start changed the key: %s", got)
	}
	// An item with response and source: both leave the body for the key.
	item := NewItemEntry(openresponses.UserText("hi"))
	item.ResponseID = "resp_1"
	item.Source = &Trigger{Kind: "cron", Ref: "nightly"}
	itemID, _ := s.Append(item)
	itemJSON, _ := json.Marshal(openresponses.UserText("hi"))
	want = step(want, TypeItem, bodyHash(`{"item":`+string(itemJSON)+`}`))
	if got, _ := s.ContextHash(itemID); got != want {
		t.Errorf("item: %s, want %s", got, want)
	}
	// A compaction: first_kept is replaced by [context hash, type] of the
	// entry it names — here the run entry, whose context hash is the
	// config's and whose type is run.
	comp := &CompactionEntry{FirstKept: runID, Summary: openresponses.SystemText("s")}
	compID, err := s.Append(comp)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, _ := s.ContextHash(runID)
	sub, _ := json.Marshal([]string{runCtx, TypeRun})
	// The compaction's body as written, with first_kept substituted: the
	// checkpoint members the entry carries are part of the body, so the
	// body is taken from the marshalled entry and only the substitution
	// is applied by hand.
	data, _ := MarshalEntry(comp)
	var all map[string]json.RawMessage
	json.Unmarshal(data, &all)
	for _, k := range []string{"id", "type", "parent", "parents", "ts"} {
		delete(all, k)
	}
	all["first_kept"] = sub
	body, _ := json.Marshal(all)
	want = step(want, TypeCompaction, bodyHash(string(body)))
	if got, _ := s.ContextHash(compID); got != want {
		t.Errorf("compaction: %s, want %s", got, want)
	}
}

// TestResolveLegacyID: an id written about a session before its file
// was migrated finds the entry, whether the file is migrated as it is
// read or was written at 0.5 with the member kept, and a current id
// resolves to itself.
func TestResolveLegacyID(t *testing.T) {
	for name, s := range map[string]*Session{
		"migrated on read": loadV04(t, "basic"),
		"0.5 fixture":      loadFixture(t, "basic"),
	} {
		t.Run(name, func(t *testing.T) {
			id := lid(t, s, "i0000004")
			if got, ok := s.Resolve("i0000004"); !ok || got != id {
				t.Errorf("Resolve(legacy) = %s, %v; want %s", got, ok, id)
			}
			if got, ok := s.Resolve(id); !ok || got != id {
				t.Errorf("Resolve(id) = %s, %v", got, ok)
			}
			for _, miss := range []string{"", "i9999999"} {
				if got, ok := s.Resolve(miss); ok {
					t.Errorf("Resolve(%q) = %s", miss, got)
				}
			}
		})
	}
}

// TestAddedMembersReadAsWritten: trigger on a run start and call_id on a
// custom entry were unknown members before 0.6, so a 0.5 file may spell
// them any way at all, and a 0.6 writer may put a member this package
// does not define inside trigger. Each such line reads, verifies and
// writes back as it was; the typed field is filled only when it holds
// the member exactly.
func TestAddedMembersReadAsWritten(t *testing.T) {
	head := `{"type":"session","format":"agentsession/0.5","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
	tests := []struct {
		name, body string
		typed      bool
	}{
		{"trigger as a string", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":"cron:nightly"`, false},
		{"trigger with a member not defined", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"kind":"schedule","due":"2026-09-17T03:00:00Z"}`, false},
		{"trigger with an empty member", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"kind":""}`, false},
		{"trigger null", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":null`, false},
		{"trigger exact", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"kind":"schedule","ref":"nightly"}`, true},
		{"call_id an object", `"type":"custom","ns":"acme","call_id":{"n":1}`, false},
		{"call_id empty", `"type":"custom","ns":"acme","call_id":""`, false},
		{"call_id exact", `"type":"custom","ns":"acme","call_id":"call_1"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := `{` + tt.body + `,"parent":null,"ts":"2026-09-17T16:00:01Z"}`
			id, _, err := EntryHashes([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			line = `{"id":"` + id + `",` + line[1:]
			s, err := Read(strings.NewReader(head + "\n" + line + "\n"))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			e, ok := s.Entry(id)
			if !ok {
				t.Fatal("entry missing")
			}
			switch v := e.(type) {
			case *RunEntry:
				if (v.Trigger != nil) != tt.typed {
					t.Errorf("Trigger typed = %v", v.Trigger != nil)
				}
			case *CustomEntry:
				if (v.CallID != "") != tt.typed {
					t.Errorf("CallID typed = %q", v.CallID)
				}
			}
			var buf bytes.Buffer
			if err := Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			again, err := Read(&buf)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if _, ok := again.Entry(id); !ok {
				t.Errorf("rewrite changed the entry's id")
			}
		})
	}
}

// TestForkRefusesUnresolvedPrefix: a migrated origin holding an entry
// the migration could not rewrite may not be re-emitted, and a fork
// whose prefix holds that entry would re-emit it under another name.
func TestForkRefusesUnresolvedPrefix(t *testing.T) {
	s := loadV04(t, "converge")
	_, unresolved := s.Migrated()
	if len(unresolved) == 0 {
		t.Fatal("the fixture has no unresolved entry; the test proves nothing")
	}
	at, ok := s.Resolve(unresolved[len(unresolved)-1])
	if !ok {
		t.Fatal("unresolved entry not found")
	}
	if _, err := Fork(s, at, Header{}); !errors.Is(err, ErrUnresolvedMigration) {
		t.Errorf("Fork over an unresolved entry = %v", err)
	}
	// Forking where the prefix holds only rewritten entries is fine.
	if _, err := Fork(s, s.Path(at)[0].Base().ID, Header{}); err != nil {
		t.Errorf("Fork at the root = %v", err)
	}
}
