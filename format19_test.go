package agentsession

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
		n, err := NewRedactionNames(key, Header{ID: c.ID, Redacted: c.Redacted}, v.Profile)
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
			if n, err := NewRedactionNames(key, Header{ID: "o", Redacted: redacted}, "p"); !errors.Is(err, ErrRedactionKey) || n != nil {
				t.Errorf("NewRedactionNames with a %d-byte key, redacted %v = %v", len(key), redacted, err)
			}
		}
	}
	key := bytes.Repeat([]byte{7}, RedactionKeySize)
	n, err := NewRedactionNames(key, Header{ID: "o"}, "p")
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
	n, _ := NewRedactionNames(key, Header{ID: orig}, "p@1")
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
	n2, _ := NewRedactionNames(other, Header{ID: orig}, "p@1")
	n3, _ := NewRedactionNames(key, Header{ID: orig}, "p@2")
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
	again, _ := NewRedactionNames(key, h, "p@1")
	if again.RedactedFrom().Session != n.ID() || again.ID() != RedactedSessionID(n.ID(), "p@1") || again.Redaction(child) != RedactedSessionID(child, "p@1") {
		t.Errorf("redaction of a redaction: from %+v, id %s", again.RedactedFrom(), again.ID())
	}
	// No profile: the pseudonym's name holds the empty string.
	bare, _ := NewRedactionNames(key, Header{ID: orig}, "")
	if p, _ := Pseudonym(key, orig, ""); bare.RedactedFrom().Session != p || bare.RedactedFrom().Profile != "" {
		t.Errorf("no profile: %+v", bare.RedactedFrom())
	}
}

// TestReplaceIDs: each session ID is replaced as a substring by its
// redaction's, a longer ID holding a shorter whole, and nothing else.
func TestReplaceIDs(t *testing.T) {
	n, _ := NewRedactionNames(testKey(t), Header{ID: "sess-1"}, "p")
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
