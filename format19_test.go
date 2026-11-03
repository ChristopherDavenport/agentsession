package agentsession

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// pseudonymVectors is testdata/pseudonym.json: the keyed pseudonym's
// test vectors (RFC 0001, The redacted session, 0.19), computed apart
// from this package with Python's hmac and uuid modules, so another
// implementation can check its derivation against them: the key, the
// profile, and for each ID the HMAC's input, the pseudonym, the UUIDv5
// name and the redacted session's ID.
type pseudonymVectors struct {
	Key     string `json:"key"`
	Profile string `json:"profile"`
	Cases   []struct {
		Note       string `json:"note"`
		ID         string `json:"id"`
		Redacted   bool   `json:"redacted"`
		HMACInput  string `json:"hmac_input"`
		Pseudonym  string `json:"pseudonym"`
		UUIDv5Name string `json:"uuidv5_name"`
		RedactedID string `json:"redacted_id"`
	} `json:"cases"`
}

// testKey is the key the pseudonym vectors use: the bytes 0 to 31.
func testKey(t *testing.T) []byte {
	t.Helper()
	var v pseudonymVectors
	raw, err := os.ReadFile(filepath.Join("testdata", "pseudonym.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	key, err := hex.DecodeString(v.Key)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestPseudonymVectors: the keyed pseudonym and the redacted session's
// ID derived from it are the vectors' for a native UUIDv4, a writer's
// UUIDv7 and a redacted session's ID, which is its own pseudonym.
func TestPseudonymVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "pseudonym.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v pseudonymVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	if len(v.Cases) != 3 {
		t.Fatalf("%d cases", len(v.Cases))
	}
	for _, c := range v.Cases {
		if !c.Redacted {
			if in := string(canonicalName("agentsession/pseudonym", c.ID, v.Profile)); in != c.HMACInput {
				t.Errorf("%s: HMAC input %s, want %s", c.Note, in, c.HMACInput)
			}
			p, err := Pseudonym(key, c.ID, v.Profile)
			if err != nil || p != c.Pseudonym {
				t.Errorf("%s: Pseudonym = %s, %v, want %s", c.Note, p, err, c.Pseudonym)
			}
			if p[14] != '8' || !strings.ContainsRune("89ab", rune(p[19])) {
				t.Errorf("%s: %s is not a UUIDv8", c.Note, p)
			}
		}
		n, err := NewRedactionNames(key, New(Header{ID: c.ID, Redacted: c.Redacted}), v.Profile)
		if err != nil {
			t.Fatal(err)
		}
		if got := n.RedactedFrom(); got.Session != c.Pseudonym || got.Profile != v.Profile {
			t.Errorf("%s: redacted_from %+v, want pseudonym %s", c.Note, got, c.Pseudonym)
		}
		if name := string(canonicalName("redacted", c.Pseudonym, v.Profile)); name != c.UUIDv5Name {
			t.Errorf("%s: UUIDv5 name %s, want %s", c.Note, name, c.UUIDv5Name)
		}
		if id := n.ID(); id != c.RedactedID || RedactedSessionID(c.Pseudonym, v.Profile) != c.RedactedID {
			t.Errorf("%s: redacted id %s, want %s", c.Note, id, c.RedactedID)
		}
	}
}

// TestRedactionKeyRefused: there is no default key, and no unkeyed
// fallback: an absent or short key is refused, for a redacted original
// too, which needs none.
func TestRedactionKeyRefused(t *testing.T) {
	for _, key := range [][]byte{nil, {}, bytes.Repeat([]byte{7}, RedactionKeySize-1)} {
		if p, err := Pseudonym(key, "o", "p"); !errors.Is(err, ErrRedactionKey) || p != "" {
			t.Errorf("Pseudonym with a %d-byte key = %q, %v", len(key), p, err)
		}
		for _, redacted := range []bool{false, true} {
			if n, err := NewRedactionNames(key, New(Header{ID: "o", Redacted: redacted}), "p"); !errors.Is(err, ErrRedactionKey) || n != nil {
				t.Errorf("NewRedactionNames with a %d-byte key, redacted %v = %v", len(key), redacted, err)
			}
		}
	}
	key := bytes.Repeat([]byte{7}, RedactionKeySize)
	n, err := NewRedactionNames(key, New(Header{ID: "o"}), "p")
	if err != nil {
		t.Fatal(err)
	}
	// The key is copied: a caller that clears its buffer changes nothing.
	before := n.ID()
	clear(key)
	if n.ID() != before {
		t.Error("RedactionNames holds the caller's key buffer")
	}
}

// TestRedactionNamesDerive: what one redaction writes is derived from
// the pseudonym under the key, so the redacted session's id is the one
// its redacted_from derives without a key, and is not the 0.18
// derivation from the original's ID; another key or another profile
// derives unrelated pseudonyms; every reference names the other
// session's redaction under the same key and profile; and a redacted
// original's IDs are their own pseudonyms.
func TestRedactionNamesDerive(t *testing.T) {
	key := testKey(t)
	const orig, child = "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "01995b2a-0000-7000-8000-000000000002"
	n, _ := NewRedactionNames(key, New(Header{ID: orig}), "p@1")
	from := n.RedactedFrom()
	if from.Session == orig || n.ID() != RedactedSessionID(from.Session, "p@1") || n.ID() == RedactedSessionID(orig, "p@1") {
		t.Errorf("redacted_from %+v, id %s", from, n.ID())
	}
	h := Header{ID: n.ID(), Redacted: true, RedactedFrom: from}
	if err := CheckRedactedID(h); err != nil {
		t.Errorf("CheckRedactedID: %v", err)
	}
	other := slices.Clone(key)
	other[0] ^= 1
	n2, _ := NewRedactionNames(other, New(Header{ID: orig}), "p@1")
	n3, _ := NewRedactionNames(key, New(Header{ID: orig}), "p@2")
	if n2.Pseudonym(orig) == from.Session || n3.Pseudonym(orig) == from.Session {
		t.Error("another key or profile derives the same pseudonym")
	}
	if got, ok := n.Session(child); !ok || got != n.Redaction(child) || got != RedactedSessionID(n.Pseudonym(child), "p@1") {
		t.Errorf("Session(%s) = %s, %v", child, got, ok)
	}
	if got, _ := n.Session(orig); got != n.ID() {
		t.Errorf("the original's own ID re-points to %s, not %s", got, n.ID())
	}
	// A redaction of the redaction derives what 0.18 derived.
	again, _ := NewRedactionNames(key, New(h), "p@1")
	keyedChild, _ := Pseudonym(key, child, "p@1")
	if again.RedactedFrom().Session != n.ID() || again.ID() != RedactedSessionID(n.ID(), "p@1") || again.Redaction(child) != RedactedSessionID(keyedChild, "p@1") {
		t.Errorf("redaction of a redaction: from %+v, id %s", again.RedactedFrom(), again.ID())
	}
	// No profile: the pseudonym's name holds the empty string.
	bare, _ := NewRedactionNames(key, New(Header{ID: orig}), "")
	if p, _ := Pseudonym(key, orig, ""); bare.RedactedFrom().Session != p || bare.RedactedFrom().Profile != "" {
		t.Errorf("no profile: %+v", bare.RedactedFrom())
	}
}

// TestReplaceIDs: each session ID is replaced as a substring by its
// redaction's, a longer ID holding a shorter whole, and nothing else.
func TestReplaceIDs(t *testing.T) {
	n, _ := NewRedactionNames(testKey(t), New(Header{ID: "sess-1"}), "p")
	f := n.ReplaceIDs([]string{"sess-1", "sess-12", "", "sess-1"})
	got := f("/tmp/sess-1/out and sess-12, not sess-2")
	want := "/tmp/" + n.Redaction("sess-1") + "/out and " + n.Redaction("sess-12") + ", not sess-2"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if f := n.ReplaceIDs(nil); f("sess-1") != "sess-1" {
		t.Error("no IDs replaced something")
	}
}

// TestSpawnedSessionID: a host-started child's derived ID, pinned so
// another implementation can check it, computed apart from this package
// as Python's uuid.uuid5(uuid.UUID(int=0), name); it differs from the
// call form's over the same strings.
func TestSpawnedSessionID(t *testing.T) {
	const parent, spawn = "01995b2a-0000-7000-8000-000000000060", "0199a000-0000-7000-8000-000000000001"
	if got, want := SpawnedSessionID(parent, spawn), "d543e939-7819-53c9-9e7e-8835d5022de3"; got != want {
		t.Errorf("SpawnedSessionID = %s, want %s", got, want)
	}
	if SpawnedSessionID(parent, spawn) == SubsessionID(parent, spawn) {
		t.Error("the spawn form derives the call form's ID")
	}
}

// TestCheckRedactedID: a redacted header whose id is not the one its
// redacted_from derives is reported; one without a profile, or not
// redacted, is not checked.
func TestCheckRedactedID(t *testing.T) {
	from := &RedactedFrom{Session: "238049a1-ae4a-823e-8000-b19bb9a2be91", Profile: "example.redact@1"}
	good := Header{ID: RedactedSessionID(from.Session, from.Profile), Redacted: true, RedactedFrom: from}
	if err := CheckRedactedID(good); err != nil {
		t.Errorf("derived id: %v", err)
	}
	bad := good
	bad.ID = "01995b2a-0000-7000-8000-000000000099"
	if err := CheckRedactedID(bad); !errors.Is(err, ErrRedactedID) {
		t.Errorf("another id: %v", err)
	}
	noProfile := bad
	noProfile.RedactedFrom = &RedactedFrom{Session: from.Session}
	if err := CheckRedactedID(noProfile); err != nil {
		t.Errorf("no profile: %v", err)
	}
	if err := CheckRedactedID(Header{ID: "x"}); err != nil {
		t.Errorf("not redacted: %v", err)
	}
}

// TestURLLinkSessionIsText: since 0.19 the session of a link carrying a
// url is the callee's ID in another system, which a redaction does not
// re-point as a reference: RemapRefs leaves it and reports nothing, and
// a rewrite reaches it as text. A link without a url is a reference, as
// before.
func TestURLLinkSessionIsText(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	remote := &LinkEntry{Rel: RelSubsession, Session: "remote-task-session", CallID: "call_1", URL: "https://agent.example/a2a"}
	local := &LinkEntry{Rel: RelSubsession, Session: "local-child", CallID: "call_2"}
	for _, l := range []*LinkEntry{remote, local} {
		l.ID, l.Parent, l.Timestamp = "x", "p", at
	}
	re := Remap{Session: func(id string) (string, bool) { return "R(" + id + ")", true }}
	got, err := RemapRefs(remote, re)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Entry.(*LinkEntry).Session; s != "remote-task-session" || len(got.Unmapped) != 0 {
		t.Errorf("a url link's session remapped to %s, unmapped %v", s, got.Unmapped)
	}
	if got, _ := RemapRefs(local, re); got.Entry.(*LinkEntry).Session != "R(local-child)" {
		t.Errorf("a local link's session is %s", got.Entry.(*LinkEntry).Session)
	}
	upper := strings.ToUpper
	out, err := RewriteStrings(remote, upper)
	if err != nil {
		t.Fatal(err)
	}
	if l := out.(*LinkEntry); l.Session != "REMOTE-TASK-SESSION" || l.CallID != "call_1" {
		t.Errorf("rewritten url link: session %s, call_id %s", l.Session, l.CallID)
	}
	if out, _ := RewriteStrings(local, upper); out.(*LinkEntry).Session != "local-child" {
		t.Errorf("a local link's session was rewritten as text: %s", out.(*LinkEntry).Session)
	}
}

// fixtures019 are the 0.19 conformance fixtures, each built natively by
// its function, as for fixtures018.
var fixtures019 = map[string]func(t *testing.T) *Session{
	"host-child":              func(t *testing.T) *Session { return hostChildFixture(t)[0] },
	"host-child-spawned":      func(t *testing.T) *Session { return hostChildFixture(t)[1] },
	"host-child-legacy":       func(t *testing.T) *Session { return hostChildLegacyFixture(t)[0] },
	"host-child-legacy-child": func(t *testing.T) *Session { return hostChildLegacyFixture(t)[1] },
	"bad-spawn-link":          func(t *testing.T) *Session { return badSpawnFixture(t, false) },
	"bad-spawn-repeated":      func(t *testing.T) *Session { return badSpawnFixture(t, true) },
	"bad-spawned-by":          badSpawnedByFixture,
	"redacted-ids-original":   func(t *testing.T) *Session { return redactedIDsOriginal(t) },
	"redacted-ids":            func(t *testing.T) *Session { return redactedIDsFixture(t, false) },
	"bad-redacted-id":         func(t *testing.T) *Session { return redactedIDsFixture(t, true) },
	"verdict":                 verdictFixture,
}

// writeFixtures019 writes the 0.19 fixtures, positive and negative.
func writeFixtures019(t *testing.T) {
	t.Helper()
	for name, build := range fixtures019 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The sessions of the host-child fixtures: a parent and the child the
// host started for it, by a link carrying hostSpawnID.
const (
	hostParent  = "01995b2a-0000-7000-8000-000000000060"
	hostSpawnID = "0199a000-0000-7000-8000-000000000001"
	hostLegacy  = "01995b2a-0000-7000-8000-000000000061"
	hostLegacyC = "01995b2a-0000-7000-8000-000000000062"
)

// answerWith appends an assistant message and its response at model,
// recording usage of n input tokens.
func answerWith(must func(Entry) string, resp, model, text string, n int) {
	must(&ItemEntry{Item: &openresponses.Message{ID: "msg_" + resp, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: text, Annotations: []openresponses.Annotation{}}}}, ResponseID: resp})
	must(&ResponseEntry{ResponseID: resp, Model: model, Status: openresponses.ResponseStatusCompleted, Usage: &openresponses.Usage{InputTokens: n, TotalTokens: n}})
}

// hostChildFixture builds host-child.jsonl and host-child-spawned.jsonl
// (RFC 0001, link, 0.19): after a turn the host starts a pass that
// consolidates memory, which no function call asked for. The parent's
// link carries spawn_id, the child's header carries it in spawned_by
// and its ID is derived over ["spawn",parent,spawn_id], and a custom
// note after the link takes the child's work in, naming its leaf in
// parents, then the person speaks again. The parent's calls record 2 and
// 1 input tokens at "priced", the child's one call 7.
func hostChildFixture(t *testing.T) [2]*Session {
	t.Helper()
	childID := SpawnedSessionID(hostParent, hostSpawnID)
	child, cmust, _ := fixtureSession(t, Header{ID: childID, ParentSession: hostParent, SpawnedBy: hostSpawnID}, "2026-11-03T15:00:10Z")
	cmust(&ConfigEntry{Model: "priced"})
	cmust(NewItemEntry(openresponses.UserText("Consolidate what this session learned.")))
	answerWith(cmust, "resp_c1", "priced", "Noted: the build uses make.", 7)

	parent, must, _ := fixtureSession(t, Header{ID: hostParent, Records: AllRecords}, "2026-11-03T15:00:00Z")
	must(&ConfigEntry{Model: "priced"})
	must(NewRunStart("run-1", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Fix the build.")))
	answerWith(must, "resp_1", "priced", "Fixed.", 2)
	e, err := parent.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(e)
	must(NewSpawnLink(childID, hostSpawnID))
	note := &CustomEntry{NS: "x.memory", Data: json.RawMessage(`{"note":"consolidated"}`)}
	note.Parents = []EntryRef{{Session: childID, Entry: child.Leaf()}}
	must(note)
	must(NewRunStart("run-2", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Thanks.")))
	answerWith(must, "resp_2", "priced", "You're welcome.", 1)
	if e, err = parent.EndRun(ReasonDone, ""); err != nil {
		t.Fatal(err)
	}
	must(e)
	return [2]*Session{parent, child}
}

// hostChildLegacyFixture builds host-child-legacy.jsonl and its child:
// a subsession link with neither call_id nor spawn_id, as a writer
// before 0.19 may have written for a child the host started, committed
// as read, since a writer of 0.19 no longer appends one; its child names
// the parent and carries no spawned_by, and a note after the link names
// its leaf.
func hostChildLegacyFixture(t *testing.T) [2]*Session {
	t.Helper()
	child, cmust, _ := fixtureSession(t, Header{ID: hostLegacyC, ParentSession: hostLegacy}, "2026-11-03T16:00:10Z")
	cmust(NewItemEntry(openresponses.UserText("Summarise the day.")))
	answerWith(cmust, "resp_l1", "priced", "A quiet day.", 5)

	parent, must, _ := fixtureSession(t, Header{ID: hostLegacy}, "2026-11-03T16:00:00Z")
	must(NewItemEntry(openresponses.UserText("Good night.")))
	if _, err := parent.Commit(NewLinkEntry(RelSubsession, hostLegacyC)); err != nil {
		t.Fatal(err)
	}
	note := &CustomEntry{NS: "x.memory", Data: json.RawMessage(`{"note":"summarised"}`)}
	note.Parents = []EntryRef{{Session: hostLegacyC, Entry: child.Leaf()}}
	must(note)
	return [2]*Session{parent, child}
}

// badSpawnFixture builds bad-spawn-link.jsonl, a subsession link
// carrying both call_id and spawn_id, and, repeated, bad-spawn-repeated:
// two links carrying one spawn_id on two branches, the second's member
// written as one the writer did not take, which a reader takes by its
// form. Neither can be appended; both are committed as a store would
// take them from a foreign file, and VerifyRecords reports each.
func badSpawnFixture(t *testing.T, repeated bool) *Session {
	t.Helper()
	id := "01995b2a-0000-7000-8000-000000000063"
	if repeated {
		id = "01995b2a-0000-7000-8000-000000000064"
	}
	s, must, _ := fixtureSession(t, Header{ID: id}, "2026-11-03T17:00:00Z")
	root := must(NewItemEntry(openresponses.UserText("Start.")))
	commit := func(e Entry) {
		t.Helper()
		if _, err := s.Commit(e); err != nil {
			t.Fatal(err)
		}
	}
	if !repeated {
		commit(&LinkEntry{Rel: RelSubsession, Session: SubsessionID(id, "call_1"), CallID: "call_1", SpawnID: "spawn-1"})
		return s
	}
	must(NewSpawnLink(SpawnedSessionID(id, "spawn-1"), "spawn-1"))
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	again := NewLinkEntry(RelSubsession, SpawnedSessionID(id, "spawn-1"))
	again.Unknown = map[string]json.RawMessage{"spawn_id": json.RawMessage(`"spawn-1"`)}
	commit(again)
	return s
}

// badSpawnedByFixture builds bad-spawned-by.jsonl: the host-child
// fixture's child, its spawned_by naming another spawn, which
// VerifyLinks reports against host-child.jsonl's link.
func badSpawnedByFixture(t *testing.T) *Session {
	t.Helper()
	s, must, _ := fixtureSession(t, Header{ID: SpawnedSessionID(hostParent, hostSpawnID), ParentSession: hostParent, SpawnedBy: "another-spawn"}, "2026-11-03T15:00:10Z")
	must(NewItemEntry(openresponses.UserText("Consolidate what this session learned.")))
	return s
}

// TestSpawnIDForms: spawn_id is taken on a subsession link as a
// non-empty string of letters, digits, _ and -, in a file of any minor,
// and kept as written on another relation or in another form; either
// is written back as read.
func TestSpawnIDForms(t *testing.T) {
	for _, tt := range []struct {
		rel, member string
		typed       bool
	}{
		{RelSubsession, `"0199a000-0000-7000-8000-000000000001"`, true},
		{RelSubsession, `"memory_pass-2"`, true},
		{RelSubsession, `"has space"`, false},
		{RelSubsession, `""`, false},
		{RelSubsession, `7`, false},
		{RelSubsession, `"é"`, false},
		{RelPeer, `"spawn-1"`, false},
		{RelForkOf, `"spawn-1"`, false},
	} {
		line := `{"type":"link","id":"x","parent":null,"ts":"2026-11-03T10:00:00Z","rel":"` + tt.rel + `","session":"s","spawn_id":` + tt.member + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s %s: %v", tt.rel, tt.member, err)
		}
		l := e.(*LinkEntry)
		_, kept := l.Unknown["spawn_id"]
		if (l.SpawnID != "") != tt.typed || kept == tt.typed {
			t.Errorf("%s %s: typed %q, kept %v", tt.rel, tt.member, l.SpawnID, kept)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, members(t, back)["spawn_id"], []byte(tt.member)) {
			t.Errorf("%s %s: written back as %s", tt.rel, tt.member, back)
		}
	}
}

// TestSpawnLinkWriter: a writer appends a subsession link carrying one
// of call_id and spawn_id, refuses one carrying both or neither, a
// spawn_id another link of the session carries on any branch, and
// spawn_id on another relation; a cancel notice of a host-started child
// carries its spawn_id and names its link.
func TestSpawnLinkWriter(t *testing.T) {
	s := New(Header{ID: "p"})
	root, err := s.Append(NewItemEntry(openresponses.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	link := NewSpawnLink(SpawnedSessionID("p", "s1"), "s1")
	if _, err := s.Append(link); err != nil {
		t.Fatalf("a spawn link: %v", err)
	}
	if _, err := s.Append(NewCancelSentLink(link)); err != nil {
		t.Errorf("a cancel notice of a spawn link: %v", err)
	}
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		e    *LinkEntry
		want error
	}{
		{"both", &LinkEntry{Rel: RelSubsession, Session: "c", CallID: "call_1", SpawnID: "s2"}, ErrSpawnLink},
		{"neither", NewLinkEntry(RelSubsession, "c"), ErrSpawnLink},
		{"repeated on another branch", NewSpawnLink("c", "s1"), ErrSpawnIDRepeated},
		{"on a peer", &LinkEntry{Rel: RelPeer, Session: "c", CallID: "call_1", SpawnID: "s3"}, nil},
		{"a cancel naming a call link with the spawn's id", &LinkEntry{Rel: RelSubsession, Session: "c", SpawnID: "s9", Target: link.ID, Cancel: CancelSent}, ErrCalleeLink},
	} {
		_, err := s.Append(tt.e)
		switch {
		case tt.want == nil && err == nil:
			t.Errorf("%s: appended", tt.name)
		case tt.want != nil && !errors.Is(err, tt.want):
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}
	if _, err := s.Append(NewSpawnLink("c2", "s2")); err != nil {
		t.Errorf("a second spawn: %v", err)
	}
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
}

// TestHostChildFixture reads the host-child fixtures: the link carries
// spawn_id and no call_id, the records verify, the child's header agrees
// with the link, and DelegatedWork counts the child at the leaf the note
// after the link names; the legacy link with neither member counts its
// child the same way and is checked by parent_session alone.
func TestHostChildFixture(t *testing.T) {
	for _, name := range []string{"host-child", "host-child-spawned", "host-child-legacy", "host-child-legacy-child"} {
		checkBuilt(t, name)
	}
	parent, child := loadFixture(t, "host-child"), loadFixture(t, "host-child-spawned")
	var link *LinkEntry
	for _, e := range parent.Entries() {
		if l, ok := e.(*LinkEntry); ok {
			link = l
		}
	}
	if link == nil || link.SpawnID != hostSpawnID || link.CallID != "" || link.Session != child.ID() {
		t.Fatalf("the link: %+v", link)
	}
	if err := parent.VerifyRecords(parent.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	resolve := func(m map[string]*Session) func(string) (*Session, error) {
		return func(id string) (*Session, error) { return m[id], nil }
	}
	if err := parent.VerifyLinks(resolve(map[string]*Session{child.ID(): child})); err != nil {
		t.Errorf("VerifyLinks: %v", err)
	}
	if turns, _ := PathInputs(parent.Path(parent.Leaf())); len(turns) != 2 {
		t.Errorf("the runs' inputs: %+v", turns)
	}
	if calls := Calls(parent.Path(parent.Leaf())); len(calls) != 0 {
		t.Errorf("the spawn link reads as a call: %+v", calls)
	}
	work, unread := DelegatedWork(parent.Path(parent.Leaf()), resolve(map[string]*Session{child.ID(): child}))
	if len(unread) != 0 || len(work) != 1 || len(work[0].Leaves) != 1 || !work[0].Leaves[0].Named || work[0].Leaves[0].Entry != child.Leaf() || len(work[0].Calls) != 1 {
		t.Errorf("delegated %+v, unread %+v", work, unread)
	}

	legacy, lchild := loadFixture(t, "host-child-legacy"), loadFixture(t, "host-child-legacy-child")
	if err := legacy.VerifyRecords(legacy.Leaf()); err != nil {
		t.Errorf("legacy VerifyRecords: %v", err)
	}
	if err := legacy.VerifyLinks(resolve(map[string]*Session{hostLegacyC: lchild})); err != nil {
		t.Errorf("legacy VerifyLinks: %v", err)
	}
	work, unread = DelegatedWork(legacy.Path(legacy.Leaf()), resolve(map[string]*Session{hostLegacyC: lchild}))
	if len(unread) != 0 || len(work) != 1 || !work[0].Leaves[0].Named || len(work[0].Calls) != 1 {
		t.Errorf("legacy delegated %+v, unread %+v", work, unread)
	}
	// A child of a link with neither member that carries a spawned_by
	// disagrees with it.
	h := lchild.Header()
	h.SpawnedBy = "spawn-x"
	other := New(h)
	if err := legacy.VerifyLinks(resolve(map[string]*Session{hostLegacyC: other})); !errors.Is(err, ErrLinkMismatch) {
		t.Errorf("legacy child with spawned_by: %v", err)
	}
}

// TestBadSpawnFixtures: a link carrying both members, and a spawn_id
// two links carry, are reported by VerifyRecords; a child whose
// spawned_by is not its link's spawn_id by VerifyLinks.
func TestBadSpawnFixtures(t *testing.T) {
	for _, name := range []string{"bad-spawn-link", "bad-spawn-repeated", "bad-spawned-by"} {
		checkBuilt(t, name)
	}
	both := loadFixture(t, "bad-spawn-link")
	if err := both.VerifyRecords(both.Leaf()); !errors.Is(err, ErrSpawnLink) {
		t.Errorf("both members: %v", err)
	}
	rep := loadFixture(t, "bad-spawn-repeated")
	if err := rep.VerifyRecords(rep.Leaf()); !errors.Is(err, ErrSpawnIDRepeated) {
		t.Errorf("repeated: %v", err)
	}
	parent, bad := loadFixture(t, "host-child"), loadFixture(t, "bad-spawned-by")
	err := parent.VerifyLinks(func(id string) (*Session, error) {
		if id == bad.ID() {
			return bad, nil
		}
		return nil, nil
	})
	if !errors.Is(err, ErrLinkMismatch) {
		t.Errorf("spawned_by: %v", err)
	}
}

// The redacted-ids fixtures' original is the pseudonym vectors' native
// session, redacted under their key and profile, so its redaction's ID
// is the vectors' first redacted_id.
const (
	idsOriginal = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	idsProfile  = "example.redact@1"
)

// redactedIDsOriginal builds redacted-ids-original.jsonl: a native
// session whose env holds a scratch path naming its own ID, which calls
// a helper by a subsession link to a child whose ID is derived from its
// own, and whose tool output quotes both IDs.
func redactedIDsOriginal(t *testing.T) *Session {
	t.Helper()
	child := SubsessionID(idsOriginal, "call_1")
	s, must, say := fixtureSession(t, Header{ID: idsOriginal, CWD: "/home/u/proj"}, "2026-11-03T18:00:00Z")
	must(&EnvEntry{CWD: "/tmp/claude-" + idsOriginal + "/scratch"})
	must(NewItemEntry(openresponses.UserText("Ask a helper to check the build.")))
	must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "task", Arguments: `{"prompt":"check the build"}`}, ResponseID: "resp_1"})
	must(&ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted})
	must(NewSubsessionLink(child, "call_1"))
	must(NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "helper "+child+" wrote /tmp/claude-"+idsOriginal+"/notes.md")))
	say("resp_2", "The helper ran in session "+child+"; notes are in its scratch directory.")
	return s
}

// redactedIDsFixture builds redacted-ids.jsonl: redacted-ids-original
// redacted as a 0.19 redactor does (RFC 0001, The redacted session,
// Every redacted entry): under the vectors' key and profile, its ID and
// redacted_from derived from the pseudonym, every reference into another
// session re-pointed to that session's redaction though the child is not
// redacted alongside, and each session ID the original names rewritten
// wherever text quotes it, the env's path and the header's cwd among
// them, to the ID the redaction writes. Every entry carries its
// position and each response the request_hash the redacted session
// rebuilds. With wrongID the header names another ID, which
// bad-redacted-id.jsonl holds and CheckRedactedID reports.
func redactedIDsFixture(t *testing.T, wrongID bool) *Session {
	t.Helper()
	orig := redactedIDsOriginal(t)
	names, err := NewRedactionNames(testKey(t), orig, idsProfile)
	if err != nil {
		t.Fatal(err)
	}
	text := names.ReplaceIDs(SessionIDsNamed(orig))
	h, err := RewriteHeaderStrings(orig.Header(), text)
	if err != nil {
		t.Fatal(err)
	}
	h.ID, h.Redacted, h.RedactedFrom = names.ID(), true, names.RedactedFrom()
	if wrongID {
		h.ID = "01995b2a-0000-7000-8000-000000000065"
	}
	red := New(h)
	rw := NewRewriter(text)
	ids := map[string]string{}
	remap := Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != orig.ID() {
			return "", true // an entry of a session not redacted here is left out
		}
		id, ok := ids[entry]
		return id, ok
	}, Session: names.Session}
	for i, e := range FileOrder(orig) {
		body, err := rw.Rewrite(e)
		if err != nil {
			t.Fatal(err)
		}
		rr, err := RemapRefs(body, remap)
		if err != nil {
			t.Fatal(err)
		}
		n := rr.Entry
		n.Base().ID = ""
		if err := SetRedacted(n, []Span{{From: i + 1, To: i + 1}}); err != nil {
			t.Fatal(err)
		}
		if resp, ok := n.(*ResponseEntry); ok && resp.RequestHash != "" {
			if err := red.SetRequestHash(resp); err != nil {
				t.Fatal(err)
			}
		}
		r, err := red.Commit(n)
		if err != nil {
			t.Fatal(err)
		}
		ids[e.Base().ID] = r.ID
	}
	return red
}

// TestRedactedIDsFixture reads redacted-ids.jsonl: it names neither its
// original's ID nor the child's, anywhere; its id is the vectors' and
// the one its redacted_from derives; the link names the child's
// redaction; the text that quoted either ID names the ID the redaction
// writes for it; and every response verifies. bad-redacted-id.jsonl,
// the same with another id, is reported by CheckRedactedID.
func TestRedactedIDsFixture(t *testing.T) {
	for _, name := range []string{"redacted-ids-original", "redacted-ids", "bad-redacted-id"} {
		checkBuilt(t, name)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "redacted-ids.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	child := SubsessionID(idsOriginal, "call_1")
	if bytes.Contains(raw, []byte(idsOriginal)) || bytes.Contains(raw, []byte(child)) {
		t.Error("the redaction still names its original or the child")
	}
	s := loadFixture(t, "redacted-ids")
	h := s.Header()
	if h.ID != "f9cf49c3-f8ed-5a2e-9ff4-fffe8e9f7a1a" || h.RedactedFrom.Session != "238049a1-ae4a-823e-8000-b19bb9a2be91" {
		t.Errorf("id %s, redacted_from %+v", h.ID, h.RedactedFrom)
	}
	if err := CheckRedactedID(h); err != nil {
		t.Errorf("CheckRedactedID: %v", err)
	}
	names, _ := NewRedactionNames(testKey(t), New(Header{ID: idsOriginal}), idsProfile)
	redChild := names.Redaction(child)
	if !bytes.Contains(raw, []byte("helper "+redChild+" wrote /tmp/claude-"+h.ID+"/notes.md")) || !bytes.Contains(raw, []byte("/tmp/claude-"+h.ID+"/scratch")) {
		t.Error("the text quoting the IDs does not name the IDs the redaction writes")
	}
	links := 0
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *LinkEntry:
			links++
			if v.Session != redChild {
				t.Errorf("the link names %s, not the child's redaction %s", v.Session, redChild)
			}
		case *ResponseEntry:
			if v.RequestHash != "" {
				if err := s.Verify(v.ID); err != nil {
					t.Errorf("response %s: %v", v.ID, err)
				}
			}
		}
		if _, ok := RedactedOf(e); !ok {
			t.Errorf("entry %s carries no redacted", e.Base().ID)
		}
	}
	if links != 1 {
		t.Errorf("%d links", links)
	}
	// The child was never redacted alongside, so a reader finds no
	// session by the link and concludes nothing from it.
	if err := s.VerifyLinks(func(string) (*Session, error) { return nil, nil }); err != nil {
		t.Errorf("VerifyLinks: %v", err)
	}
	bad := loadFixture(t, "bad-redacted-id")
	if err := CheckRedactedID(bad.Header()); !errors.Is(err, ErrRedactedID) {
		t.Errorf("bad-redacted-id: %v", err)
	}
}

// verdictFixture builds verdict.jsonl (RFC 0001, The redacted session,
// 0.19): a scan's verdict on redacted-ids.jsonl, written as an outcome
// in a fork of it whose base is the third entry, the last judged, and
// whose parent_session is the redacted session's id. Its details name
// the kind and count of a finding and never its text. The fork carries
// no redacted, and its prefix entries carry theirs.
func verdictFixture(t *testing.T) *Session {
	t.Helper()
	red := redactedIDsFixture(t, false)
	base := FileOrder(red)[2].Base().ID
	created, _ := time.Parse(time.RFC3339, "2026-11-03T19:00:00Z")
	fork, err := Fork(red, base, Header{ID: "01995b2a-0000-7000-8000-000000000066", CreatedAt: created, Harness: &Harness{Name: "fixture-scan", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	pass := false
	verdict := &OutcomeEntry{Kind: "test", Target: base, Pass: &pass, Details: json.RawMessage(`{"agentredact:findings":{"aws-key":1}}`)}
	verdict.Timestamp = created.Add(time.Second)
	if _, err := fork.Append(verdict); err != nil {
		t.Fatal(err)
	}
	return fork
}

// TestVerdictFixture reads verdict.jsonl: a fork of the redacted session
// at the judged entry, not itself redacted, whose prefix entries carry
// redacted, holding the outcome naming its base; a store that holds
// only redacted entries refuses it.
func TestVerdictFixture(t *testing.T) {
	checkBuilt(t, "verdict")
	s := loadFixture(t, "verdict")
	h := s.Header()
	red := loadFixture(t, "redacted-ids")
	if h.Redacted || h.RedactedFrom != nil || h.ParentSession != red.ID() || h.Base != FileOrder(red)[2].Base().ID {
		t.Errorf("header %+v", h)
	}
	var outcome *OutcomeEntry
	for _, e := range s.Entries() {
		_, marked := RedactedOf(e)
		switch {
		case s.Prefix(e.Base().ID) && !marked:
			t.Errorf("prefix entry %s carries no redacted", e.Base().ID)
		case !s.Prefix(e.Base().ID) && marked:
			t.Errorf("own entry %s carries redacted", e.Base().ID)
		}
		if o, ok := e.(*OutcomeEntry); ok {
			outcome = o
		}
	}
	if outcome == nil || outcome.Target != h.Base || outcome.Pass == nil || *outcome.Pass {
		t.Errorf("outcome %+v", outcome)
	}
	if err := CheckCreate(h, true); !errors.Is(err, ErrNotRedacted) {
		t.Errorf("a redacted-only store's create: %v", err)
	}
}

// TestRemapRefsUnmappedIntoRepointedSession: a remap that re-points a
// session and leaves an entry reference into it as written, a parents
// element's entry or a copied_from's, whether its Entry callback leaves
// it or there is none, would name an original's entry hash inside the
// redaction, so RemapRefs refuses it, for an extension entry too; one
// that maps or drops the entry, or leaves the session too, passes.
func TestRemapRefsUnmappedIntoRepointedSession(t *testing.T) {
	n, _ := NewRedactionNames(testKey(t), New(Header{ID: "o"}), "p")
	core := NewItemEntry(openresponses.UserText("hi"))
	core.ID, core.Parent, core.Timestamp = "x", "p1", fixedTime
	core.Parents = []EntryRef{{Session: "S", Entry: "sha256:X"}}
	copied := &ItemEntry{Item: openresponses.UserText("hi"), CopiedFrom: &CopiedFrom{Session: "S", Entry: "sha256:X"}}
	copied.ID, copied.Timestamp = "y", fixedTime
	ext, err := UnmarshalEntry([]byte(`{"type":"x:note","id":"z","parent":"p1","ts":"2026-11-03T10:00:00Z","parents":[{"entry":"sha256:X","session":"S"}],"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	leave := func(string, string) (string, bool) { return "", false }
	drop := func(string, string) (string, bool) { return "", true }
	for _, e := range []Entry{core, copied, ext} {
		for name, m := range map[string]Remap{
			"entry left": {Entry: leave, Session: n.Session},
			"no Entry":   {Session: n.Session},
		} {
			if _, err := RemapRefs(e, m); !errors.Is(err, ErrUnmappedCopy) {
				t.Errorf("%s, %s: %v, want ErrUnmappedCopy", e.EntryType(), name, err)
			}
		}
		if _, err := RemapRefs(e, Remap{Entry: leave}); err != nil {
			t.Errorf("%s, session left too: %v", e.EntryType(), err)
		}
		if _, err := RemapRefs(e, Remap{Entry: drop, Session: n.Session}); err != nil {
			t.Errorf("%s, entry dropped: %v", e.EntryType(), err)
		}
	}
}

// TestRedactionNamesPerID: whether an ID is its own pseudonym is decided
// of each ID apart (RFC 0001, The redacted session, 0.19). A resumed
// redacted session names a child by a redacted link and another by a
// link appended natively: a redaction of it derives the first unkeyed,
// as 0.18 did, and keys the second, which was never published as it
// stands. A native fork of a redacted session R names R in
// parent_session: read through WithResolve, R is a redaction already and
// the reference keeps R's ID; unread, it is keyed like any other.
func TestRedactionNamesPerID(t *testing.T) {
	key := testKey(t)
	const p = "p@1"
	r := New(Header{ID: "red-r", Redacted: true, RedactedFrom: &RedactedFrom{Session: "pseudo", Profile: p}})
	first := NewSpawnLink("child-redacted", "s1")
	if err := SetRedacted(first, []Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Append(NewSpawnLink("child-native", "s2")); err != nil {
		t.Fatal(err)
	}
	n, err := NewRedactionNames(key, r, p)
	if err != nil {
		t.Fatal(err)
	}
	keyed := func(id string) string { k, _ := Pseudonym(key, id, p); return RedactedSessionID(k, p) }
	if got := n.Redaction("child-redacted"); got != RedactedSessionID("child-redacted", p) {
		t.Errorf("a child a redacted entry names: %s", got)
	}
	if got := n.Redaction("child-native"); got != keyed("child-native") {
		t.Errorf("a child only a native entry names: %s, want it keyed %s", got, keyed("child-native"))
	}
	if n.ID() != RedactedSessionID("red-r", p) {
		t.Errorf("the redacted original's own ID: %s", n.ID())
	}

	fork := New(Header{ID: "fork-f", ParentSession: "red-r"})
	if _, err := fork.Append(NewItemEntry(openresponses.UserText("more"))); err != nil {
		t.Fatal(err)
	}
	unread, _ := NewRedactionNames(key, fork, p)
	if got := unread.Redaction("red-r"); got != keyed("red-r") {
		t.Errorf("unread R: %s, want it keyed", got)
	}
	read, _ := NewRedactionNames(key, fork, p)
	read.WithResolve(func(id string) (*Session, error) {
		if id == "red-r" {
			return r, nil
		}
		return nil, nil
	})
	if got := read.Redaction("red-r"); got != "red-r" {
		t.Errorf("readable redacted R: %s, want R's own ID", got)
	}
	if read.ID() != keyed("fork-f") || read.RedactedFrom().Session == "fork-f" {
		t.Errorf("the native fork's own ID: %s", read.ID())
	}
	h, _ := RemapHeaderRefs(fork.Header(), Remap{Session: read.Session})
	if h.ParentSession != "red-r" {
		t.Errorf("parent_session re-pointed to %s", h.ParentSession)
	}
}

// TestIDTextExemption: RFC 0001's rule that rewrites session IDs in
// text exempts redacted_from, legacy_id and redacted alone, and says a
// normalised record is rewritten with the member it describes, as
// RewriteStrings does; a port reading "every history member" would keep
// the original's ID in a was or a raw.
func TestIDTextExemption(t *testing.T) {
	rfc, err := os.ReadFile(filepath.Join("docs", "rfcs", "0001-agent-session-format.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(rfc)), " ")
	for _, want := range []string{
		"It MUST NOT rewrite `redacted_from`, `legacy_id` or `redacted` by this rule",
		"a `normalised` record is rewritten with the member it describes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("RFC 0001 does not say %q", want)
		}
	}
}

// TestCalleesSkipHostStarted: a link with spawn_id belongs to no call
// (RFC 0001, link, 0.19), nor does a subsession link with neither
// member, so Callees, which lists the callees of the path's calls,
// lists neither; a call's subsession link it lists as before.
func TestCalleesSkipHostStarted(t *testing.T) {
	parent := hostChildFixture(t)[0]
	legacy := hostChildLegacyFixture(t)[0]
	for _, s := range []*Session{parent, legacy} {
		if got := Callees(s.Path(s.Leaf())); len(got) != 0 {
			t.Errorf("%s: callees %+v", s.ID(), got)
		}
	}
	s := New(Header{ID: "p"})
	if _, err := s.Append(NewSubsessionLink("c", "call_1")); err != nil {
		t.Fatal(err)
	}
	if got := Callees(s.Path(s.Leaf())); len(got) != 1 {
		t.Errorf("a call's link: %+v", got)
	}
}

// TestRedactionNamesHideTheKey: printing a RedactionNames, by %v, %+v,
// %#v or through log/slog, never shows its key.
func TestRedactionNamesHideTheKey(t *testing.T) {
	key := []byte("a very secret redaction key, 32+")
	n, err := NewRedactionNames(key, New(Header{ID: "orig"}), "p@1")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("redacting", "names", n)
	for _, out := range []string{fmt.Sprintf("%v", n), fmt.Sprintf("%+v", n), fmt.Sprintf("%#v", n), buf.String()} {
		if strings.Contains(out, "secret") || strings.Contains(out, fmt.Sprint(key)) || !strings.Contains(out, "orig") {
			t.Errorf("printed as %s", out)
		}
	}
}
