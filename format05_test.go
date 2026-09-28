package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
// writes back as it was; the typed field is filled whenever the member
// decodes into it, and what it cannot hold is kept as read.
func TestAddedMembersReadAsWritten(t *testing.T) {
	head := `{"type":"session","format":"agentsession/0.5","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
	tests := []struct {
		name, body string
		typed      bool
	}{
		{"trigger as a string", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":"cron:nightly"`, false},
		{"trigger with a member not defined", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"kind":"schedule","due":"2026-09-17T03:00:00Z"}`, true},
		{"trigger with an empty member", `"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"kind":""}`, true},
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

// TestNestedMembersReadAsWritten: a member nested inside an object this
// package types, which the format has a reader preserve, is hashed as
// the line holds it and written back as read. A file another writer
// hashed correctly reads; one whose nested member was edited without
// its id does not; and a caller that changes the typed field has
// changed the member.
func TestNestedMembersReadAsWritten(t *testing.T) {
	head := `{"type":"session","format":"agentsession/0.6","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
	hashed := func(body string) string {
		t.Helper()
		line := `{` + body + `,"parent":null,"ts":"2026-09-17T16:00:01Z"}`
		id, _, err := EntryHashes([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		return `{"id":"` + id + `",` + line[1:]
	}
	for name, body := range map[string]string{
		"workspace host": `"type":"env","cwd":"/w","workspace":{"kind":"container","ref":"sha256:ab","host":"build-7"}`,
		"queued trigger": `"type":"queued","mode":"steer","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},"trigger":{"kind":"human","seat":2}`,
		"parents":        `"type":"info","name":"n","parents":[{"session":"other","entry":"x","note":"keep-me"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			line := hashed(body)
			s, err := Read(strings.NewReader(head + "\n" + line + "\n"))
			if err != nil {
				t.Fatalf("Read of a correctly hashed line: %v", err)
			}
			var buf bytes.Buffer
			if err := Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			var want, got map[string]any
			json.Unmarshal([]byte(line), &want)
			json.Unmarshal([]byte(lines[1]), &got)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("rewrite changed the line\nread  %s\nwrote %s", line, lines[1])
			}
			// The same entry appended to another session hashes the same.
			e := s.Entries()[0]
			other := New(Header{})
			if id, err := other.Append(e); err != nil || id != e.Base().ID {
				t.Errorf("re-append = %s, %v; want %s", id, err, e.Base().ID)
			}
		})
	}
	t.Run("tampered nested member", func(t *testing.T) {
		line := strings.Replace(hashed(`"type":"env","cwd":"/w","workspace":{"kind":"container","ref":"sha256:ab","host":"build-7"}`), "build-7", "build-8", 1)
		if _, err := Read(strings.NewReader(head + "\n" + line + "\n")); !errors.Is(err, ErrBadID) {
			t.Errorf("Read of an edited nested member = %v, want ErrBadID", err)
		}
	})
	// A key in another case is a member the format does not define:
	// Go's decoder would match it to the member it resembles, and a
	// conforming reader ignores it, so it is kept and not read.
	for name, tt := range map[string]struct {
		body  string
		check func(Entry) bool
	}{
		"reason in another case": {`"type":"run","run_id":"r","phase":"end","reason":"done","Reason":"aborted","pending":[]`,
			func(e Entry) bool { return e.(*RunEntry).Reason == ReasonDone }},
		"cwd in another case": {`"type":"env","CWD":"/etc"`,
			func(e Entry) bool { return e.(*EnvEntry).CWD == "" }},
		"kind in another case": {`"type":"env","cwd":"/w","workspace":{"kind":"local","KIND":"container"}`,
			func(e Entry) bool { return e.(*EnvEntry).Workspace.Kind == WorkspaceLocal }},
		"folded key first": {`"type":"env","cwd":"/w","workspace":{"KIND":"container","kind":"local"}`,
			func(e Entry) bool { return e.(*EnvEntry).Workspace.Kind == WorkspaceLocal }},
	} {
		t.Run("folded: "+name, func(t *testing.T) {
			line := hashed(tt.body)
			s, err := Read(strings.NewReader(head + "\n" + line + "\n"))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			e := s.Entries()[0]
			if !tt.check(e) {
				t.Errorf("read the folded key as the member: %+v", e)
			}
			var buf bytes.Buffer
			if err := Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			again, err := Read(&buf)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if _, ok := again.Entry(e.Base().ID); !ok {
				t.Error("rewrite changed the id")
			}
		})
	}
	// What the reader does not read as written stays refused.
	for name, body := range map[string]string{
		"run end without pending":   `"type":"run","run_id":"r","phase":"end","reason":"done"`,
		"label without target":      `"type":"label","label":"x"`,
		"compaction, no first_kept": `"type":"compaction","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"s"}]}`,
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			if _, err := Read(strings.NewReader(head + "\n" + hashed(body) + "\n")); err == nil {
				t.Error("Read accepted a line the reader does not read as written")
			}
		})
	}
	t.Run("a trigger in another case stays unknown", func(t *testing.T) {
		s, err := Read(strings.NewReader(head + "\n" + hashed(`"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"KIND":"schedule"}`) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if r := s.Entries()[0].(*RunEntry); r.Trigger != nil {
			t.Errorf("Trigger = %+v, want nil: no conforming reader sees a kind", r.Trigger)
		}
	})
	t.Run("a caller's change keeps what it did not touch", func(t *testing.T) {
		line := hashed(`"type":"env","cwd":"/w","workspace":{"kind":"container","ref":"sha256:ab","host":"build-7"}`)
		s, err := Read(strings.NewReader(head + "\n" + line + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		env := s.Entries()[0].(*EnvEntry)
		changed := *env
		changed.Workspace = &Workspace{Kind: WorkspaceLocal}
		changed.CWD = "/elsewhere"
		data, err := MarshalEntry(&changed)
		if err != nil {
			t.Fatal(err)
		}
		// The caller's kind and the reader's host: the change is the
		// caller's, and the host is a member no field of this package
		// holds, which a rewriter preserves.
		if !strings.Contains(string(data), `"host":"build-7"`) || !strings.Contains(string(data), `"kind":"local"`) || strings.Contains(string(data), "sha256:ab") {
			t.Errorf("changed workspace written as %s", data)
		}
	})
}

// TestMigrationKeepsNestedMembers: a 0.4 entry is rewritten and rehashed
// on read, and a member nested in an object this package types survives
// that as it survives a 0.5 read.
func TestMigrationKeepsNestedMembers(t *testing.T) {
	in := `{"type":"session","format":"agentsession/0.4","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}
{"type":"env","id":"e1","parent":null,"ts":"2026-09-17T16:00:01Z","cwd":"/w","workspace":{"kind":"container","ref":"sha256:ab","host":"build-7"}}
{"type":"info","id":"e2","parent":"e1","ts":"2026-09-17T16:00:02Z","name":"n"}
{"type":"info","id":"e3","parent":"e2","ts":"2026-09-17T16:00:03Z","name":"m","parents":[{"entry":"e1","note":"keep-me"}]}
`
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"host":"build-7"`) || !strings.Contains(buf.String(), `"legacy_id":"e1"`) || !strings.Contains(buf.String(), `"note":"keep-me"`) {
		t.Errorf("migration lost the nested member or the legacy id:\n%s", buf.String())
	}
	if _, err := Read(&buf); err != nil {
		t.Errorf("migrated file reads back: %v", err)
	}
}

// TestKeptArrayExtrasFollowTheirElement: an extra member of an array
// element belongs to that element. When Append sorts parents it stays
// on the reference it described, and a reference the caller replaces
// does not inherit it.
func TestKeptArrayExtrasFollowTheirElement(t *testing.T) {
	in := `{"type":"session","format":"agentsession/0.4","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}
{"type":"info","id":"e0","parent":null,"ts":"2026-09-17T16:00:00Z","name":"a"}
{"type":"info","id":"e1","parent":"e0","ts":"2026-09-17T16:00:01Z","name":"b"}
{"type":"info","id":"e3","parent":"e1","ts":"2026-09-17T16:00:02Z","name":"c"}
{"type":"info","id":"e2","parent":"e3","ts":"2026-09-17T16:00:03Z","name":"n","parents":[{"entry":"e1","note":"keep-me"},{"entry":"e0"}]}
`
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := s.Resolve("e1")
	e0, _ := s.Resolve("e0")
	orig := s.Entries()[3].(*InfoEntry)
	noteOn := func(data []byte) string {
		t.Helper()
		var line struct {
			Parents []map[string]any `json:"parents"`
		}
		if err := json.Unmarshal(data, &line); err != nil {
			t.Fatal(err)
		}
		for _, p := range line.Parents {
			if p["note"] == "keep-me" {
				return p["entry"].(string)
			}
		}
		return ""
	}
	// Append sorts parents: the note stays on e1.
	c := *orig
	c.ID, c.Name = "", "changed"
	if _, err := s.Append(&c); err != nil {
		t.Fatal(err)
	}
	data, err := MarshalEntry(&c)
	if err != nil {
		t.Fatal(err)
	}
	if got := noteOn(data); got != e1 {
		t.Errorf("after sorting the note is on %s, want e1 %s:\n%s", got, e1, data)
	}
	// A replaced reference is the caller's: it takes no note.
	r := *orig
	r.ID = ""
	r.Parents = []EntryRef{{Entry: e0}, {Session: "other", Entry: "x"}}
	data, err = MarshalEntry(&r)
	if err != nil {
		t.Fatal(err)
	}
	if got := noteOn(data); got != "" {
		t.Errorf("a replaced reference inherited the note, on %s:\n%s", got, data)
	}
}

// TestThirdReviewShapes: the shapes a third review of #89 found. A
// folded key beside the member it resembles is read as a conforming
// reader reads it, at the top level and nested, even where the decoder
// then leaves the member empty; a null a third-party type adds where the
// line has nothing is the same line; and a changed array element does
// not take another element's extras.
func TestThirdReviewShapes(t *testing.T) {
	head := `{"type":"session","format":"agentsession/0.6","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
	read := func(t *testing.T, body string) Entry {
		t.Helper()
		line := `{` + body + `,"parent":null,"ts":"2026-09-17T16:00:01Z"}`
		id, _, err := EntryHashes([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		line = `{"id":"` + id + `",` + line[1:]
		s, err := Read(strings.NewReader(head + "\n" + line + "\n"))
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		var buf bytes.Buffer
		if err := Write(&buf, s); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		var want, got map[string]any
		json.Unmarshal([]byte(line), &want)
		json.Unmarshal([]byte(lines[1]), &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("rewrite changed the line\nread  %s\nwrote %s", line, lines[1])
		}
		return s.Entries()[0]
	}
	t.Run("nested folded key after the member", func(t *testing.T) {
		e := read(t, `"type":"env","cwd":"/w","workspace":{"kind":"container","ref":"x","REF":""}`).(*EnvEntry)
		if e.Workspace.Ref != "x" {
			t.Errorf("Ref = %q, want x", e.Workspace.Ref)
		}
	})
	t.Run("folded key after the member in a trigger", func(t *testing.T) {
		e := read(t, `"type":"run","run_id":"r","phase":"start","source":"input","trigger":{"kind":"k","ref":"r","REF":""}`).(*RunEntry)
		if e.Trigger == nil || e.Trigger.Ref != "r" {
			t.Errorf("Trigger = %+v, want ref r", e.Trigger)
		}
	})
	t.Run("top-level folded key after the member", func(t *testing.T) {
		e := read(t, `"type":"env","cwd":"/w","CWD":""`).(*EnvEntry)
		if e.CWD != "/w" {
			t.Errorf("CWD = %q, want /w", e.CWD)
		}
	})
	t.Run("a null the payload type adds", func(t *testing.T) {
		read(t, `"type":"config","model":"m","reasoning":{"effort":"low","zz":1}`)
	})
	t.Run("a changed element keeps its own extras", func(t *testing.T) {
		raw := []any{map[string]any{"p": "a", "x": json.Number("1")}, map[string]any{"p": "b", "x": json.Number("2")}}
		seen := []any{map[string]any{"p": "a"}, map[string]any{"p": "b"}}
		cur := []any{map[string]any{"p": "b"}, map[string]any{"p": "b"}}
		got := overlay(cur, raw, seen).([]any)
		if x := got[1].(map[string]any)["x"]; x != json.Number("2") {
			t.Errorf("the unchanged element lost its extra: %v", got)
		}
		if _, ok := got[0].(map[string]any)["x"]; ok {
			t.Errorf("the changed element took an extra: %v", got)
		}
	})
}
