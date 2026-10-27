package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testRedacted exercises RFC 0002's redacted sessions, which a store
// treats as any other: created, appended to with redacted entries as
// the original grows, resumed with native entries, imported and pushed
// again grown; a store set to hold only redacted entries refuses a
// header without redacted and every entry without it, by every path it
// has; [agentsession.Original] maps redacted entries back by position,
// and [agentsession.Redactions] finds what was made from what. A fork
// whose prefix holds a redacted member from before 0.13 is refused, as
// the fork would give it a meaning it was not written with.
func testRedacted(t *testing.T, opts Options) {
	t.Run("CreateAndGrow", func(t *testing.T) { redactedGrow(t, opts) })
	t.Run("CreateMalformed", func(t *testing.T) { redactedCreateMalformed(t, opts) })
	t.Run("Import", func(t *testing.T) { redactedImport(t, opts) })
	t.Run("PushGrown", func(t *testing.T) { redactedPushGrown(t, opts) })
	t.Run("RedactedOnly", func(t *testing.T) { redactedOnly(t, opts) })
	t.Run("Original", func(t *testing.T) { redactedOriginal(t, opts) })
	t.Run("OriginalOfAFork", func(t *testing.T) { redactedOriginalFork(t, opts) })
	t.Run("Redactions", func(t *testing.T) { redactedRedactions(t, opts) })
	t.Run("ForkOverAnOldRedacted", func(t *testing.T) { redactedForkRaise(t, opts) })
}

// secret is what the redactions here take out.
const secret = "hunter2"

func hide(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") }

// original builds, in memory, a session holding the secret on more
// than one branch, with a label naming an entry and a response whose
// request holds the secret, so that a redaction rewrites bodies,
// re-points parents and targets, and gives the response the
// request_hash of the request it rebuilds.
func original(t *testing.T, id string) *agentsession.Session {
	t.Helper()
	s := agentsession.New(agentsession.Header{ID: id, CWD: "/w"})
	root, err := s.Append(user("my password is " + secret))
	if err != nil {
		t.Fatal(err)
	}
	reply := agentsession.NewItemEntry(openresponses.AssistantText("noted: " + secret))
	reply.ResponseID = "resp-1"
	if _, err := s.Append(reply); err != nil {
		t.Fatal(err)
	}
	resp := &agentsession.ResponseEntry{ResponseID: "resp-1", Status: openresponses.ResponseStatusCompleted, RequestHash: "sha256:to-compute"}
	if err := s.SetRequestHash(resp); err != nil {
		t.Fatal(err)
	}
	r, err := s.Append(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(r); err != nil {
		t.Fatalf("the original's response does not verify: %v", err)
	}
	other := agentsession.NewItemEntry(openresponses.UserText("forget " + secret))
	other.Parent = root
	b, err := s.Append(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(agentsession.NewLabelEntry(b, "kept")); err != nil {
		t.Fatal(err)
	}
	return s
}

// redactor redacts an original into a session of its own, in memory,
// with the exported writer helpers, as a redactor does: each entry the
// original holds past those it has done, in file order, through one
// Rewriter, its references re-pointed with RemapRefs to the entries
// already written, marked with SetRedacted with its position, a
// response given the request_hash of its request in the redacted
// session with SetRequestHash, its ID cleared, and committed. Called
// again as the original grows, it appends the rest, so the redacted
// session grows with it.
type redactor struct {
	orig string
	red  *agentsession.Session
	rw   *agentsession.Rewriter
	ids  map[string]string
	done int
}

func newRedactor(orig *agentsession.Session, profile string, h agentsession.Header) *redactor {
	oh := orig.Header()
	if h.ID == "" {
		h.ID = agentsession.RedactedSessionID(oh.ID, profile)
	}
	h.CWD, h.Redacted = oh.CWD, true
	h.RedactedFrom = &agentsession.RedactedFrom{Session: oh.ID, Profile: profile}
	return &redactor{orig: oh.ID, red: agentsession.New(h), rw: agentsession.NewRewriter(hide), ids: map[string]string{}}
}

// redact redacts what orig holds past what was done and returns the
// entries it wrote, IDs set.
func (r *redactor) redact(t *testing.T, orig *agentsession.Session) []agentsession.Entry {
	t.Helper()
	remap := agentsession.Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != r.orig {
			return "", false
		}
		id, ok := r.ids[entry]
		return id, ok
	}}
	order := agentsession.FileOrder(orig)
	var out []agentsession.Entry
	for i := r.done; i < len(order); i++ {
		e := order[i]
		body, err := r.rw.Rewrite(e)
		if err != nil {
			t.Fatalf("rewrite %s: %v", e.Base().ID, err)
		}
		rr, err := agentsession.RemapRefs(body, remap)
		if err != nil {
			t.Fatalf("remap %s: %v", e.Base().ID, err)
		}
		n := rr.Entry
		n.Base().ID = ""
		if err := agentsession.SetRedacted(n, []agentsession.Span{{From: i + 1, To: i + 1}}); err != nil {
			t.Fatal(err)
		}
		if resp, ok := n.(*agentsession.ResponseEntry); ok {
			if err := r.red.SetRequestHash(resp); err != nil {
				t.Fatalf("request hash of %s: %v", e.Base().ID, err)
			}
		}
		res, err := r.red.Commit(n)
		if err != nil {
			t.Fatalf("commit %s: %v", e.Base().ID, err)
		}
		r.ids[e.Base().ID] = res.ID
		out = append(out, n)
	}
	r.done = len(order)
	return out
}

// redaction is a whole redaction of orig under profile, and its file.
func redaction(t *testing.T, orig *agentsession.Session, profile string) (*agentsession.Session, []byte) {
	t.Helper()
	r := newRedactor(orig, profile, agentsession.Header{})
	r.redact(t, orig)
	file := fileOf(t, r.red)
	if strings.Contains(string(file), secret) {
		t.Fatalf("the redaction kept the secret:\n%s", file)
	}
	s, err := agentsession.Read(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("the redacted file does not read: %v\n%s", err, file)
	}
	if responsesVerify(t, s) == 0 {
		t.Fatal("the redaction holds no response with a request_hash; the case proves nothing")
	}
	return s, file
}

// responsesVerify checks that every response of s that carries a
// request_hash verifies, as in any session: a redacted session's hashes
// are its own (RFC 0001, Redaction). It returns how many it checked.
func responsesVerify(t *testing.T, s *agentsession.Session) int {
	t.Helper()
	n := 0
	for _, e := range s.Entries() {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok || r.RequestHash == "" {
			continue
		}
		n++
		if err := s.Verify(r.ID); err != nil {
			t.Errorf("response %s of %s does not verify: %v", r.ID, s.ID(), err)
		}
	}
	return n
}

// clone is a copy of an entry, its ID and parent set, as a store is
// given one.
func clone(t *testing.T, e agentsession.Entry) agentsession.Entry {
	t.Helper()
	b, err := agentsession.MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	c, err := agentsession.UnmarshalEntry(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fileOf writes a session as its file.
func fileOf(t *testing.T, s *agentsession.Session) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readBack reads a session the store holds, without a hold where it can.
func readBack(t *testing.T, st agentsession.Store, id string) *agentsession.Session {
	t.Helper()
	var (
		s   *agentsession.Session
		err error
	)
	if r, ok := st.(agentsession.Reader); ok {
		s, err = r.Read(context.Background(), id)
	} else {
		s, err = st.Open(context.Background(), id)
	}
	if err != nil {
		t.Fatalf("read %s back: %v", id, err)
	}
	return s
}

// sameRedacted checks that the store holds want: redacted, linked to its
// original, its entries in file order, each redacted where want's is,
// and verifying when its file is read again, every response's
// request_hash among it.
func sameRedacted(t *testing.T, st agentsession.Store, want *agentsession.Session) {
	t.Helper()
	got := readBack(t, st, want.ID())
	h := got.Header()
	if !h.Redacted || !reflect.DeepEqual(h.RedactedFrom, want.Header().RedactedFrom) {
		t.Errorf("held header: redacted %v, redacted_from %+v; want %+v", h.Redacted, h.RedactedFrom, want.Header().RedactedFrom)
	}
	if g, w := entryIDs(withoutKeptHeads(got, want)), entryIDs(want); !reflect.DeepEqual(g, w) {
		t.Errorf("held entries %v, want %v", g, w)
	}
	for _, e := range want.Entries() {
		ws, wok := agentsession.RedactedOf(e)
		g, _ := got.Entry(e.Base().ID)
		if g == nil {
			continue
		}
		if gs, gok := agentsession.RedactedOf(g); gok != wok || !reflect.DeepEqual(gs, ws) {
			t.Errorf("held entry %s redacted %v %v, want %v %v", e.Base().ID, gs, gok, ws, wok)
		}
	}
	back, err := agentsession.Read(bytes.NewReader(fileOf(t, got)))
	if err != nil {
		t.Errorf("the held session does not verify: %v", err)
		return
	}
	responsesVerify(t, back)
}

// importer is how a case takes a session file into st whole: the
// Import hook when the store gives one, else the store's own
// [agentsession.Importer], else nil.
func importer(opts Options, st agentsession.Store) func(t *testing.T, s agentsession.Store, file []byte) error {
	if opts.Import != nil {
		return opts.Import
	}
	if im, ok := st.(agentsession.Importer); ok {
		return func(t *testing.T, s agentsession.Store, file []byte) error {
			_, err := im.Import(context.Background(), bytes.NewReader(file), true)
			return err
		}
	}
	return nil
}

// hooks skips a case that needs a hook the store lacks.
func hooks(t *testing.T, have ...bool) {
	t.Helper()
	for _, ok := range have {
		if !ok {
			t.Skip("the store has no such path")
		}
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

// withoutKeptHeads is got without the leaf labels the store appended to
// keep its head where a branch left it (RFC 0002, Head), those want
// does not hold: a session of the entries a writer appended.
func withoutKeptHeads(got, want *agentsession.Session) *agentsession.Session {
	out := agentsession.New(got.Header())
	for _, e := range got.Entries() {
		if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel {
			if _, held := want.Entry(e.Base().ID); !held {
				continue
			}
		}
		_, _ = out.Extend(e)
	}
	return out
}

// appendAll appends entries to the session id in st, as given.
func appendAll(t *testing.T, st agentsession.Store, id string, entries []agentsession.Entry) {
	t.Helper()
	for _, e := range entries {
		if _, err := st.Append(context.Background(), id, clone(t, e)); err != nil {
			t.Fatalf("append %s: %v", e.Base().ID, err)
		}
	}
}

// redactedGrow: a redacted session is created as any other, takes the
// redacted entries its redactor appends as the original grows, and is
// resumed by a writer whose entry is native, which it takes too.
func redactedGrow(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	orig := original(t, "orig-grow")
	r := newRedactor(orig, "test.secret@1", agentsession.Header{})
	if _, err := st.Create(ctx, r.red.Header()); err != nil {
		t.Fatalf("Create of a redacted session: %v", err)
	}
	appendAll(t, st, r.red.ID(), r.redact(t, orig))
	if _, err := orig.Append(user("later, " + secret)); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, r.red.ID(), r.redact(t, orig))
	sameRedacted(t, st, r.red)

	native := user("resumed here")
	id, err := st.Append(ctx, r.red.ID(), native)
	if err != nil {
		t.Fatalf("a native entry on a redacted session: %v", err)
	}
	got := readBack(t, st, r.red.ID())
	e, ok := got.Entry(id)
	if n := len(withoutKeptHeads(got, r.red).Entries()); !ok || n != r.red.Len()+1 {
		t.Fatalf("after the native append the session holds %d entries", n)
	}
	if _, redacted := agentsession.RedactedOf(e); redacted {
		t.Error("the native entry reads back redacted")
	}
	if opts.Reopen != nil {
		if got := readBack(t, opts.Reopen(t, st), r.red.ID()); len(withoutKeptHeads(got, r.red).Entries()) != r.red.Len()+1 {
			t.Errorf("after a reopen the session holds %d entries", got.Len())
		}
	}
}

// redactedImport: a redacted file is held as written and verifies,
// beside the original, and grows by append afterwards, redacted and
// native.
func redactedImport(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	orig := original(t, "orig-import")
	r := newRedactor(orig, "test.secret@1", agentsession.Header{})
	r.redact(t, orig)
	if err := take(t, st, fileOf(t, orig)); err != nil {
		t.Fatalf("import the original: %v", err)
	}
	if err := take(t, st, fileOf(t, r.red)); err != nil {
		t.Fatalf("import the redaction: %v", err)
	}
	sameRedacted(t, st, r.red)
	if _, err := orig.Append(user("more, " + secret)); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, r.red.ID(), r.redact(t, orig))
	sameRedacted(t, st, r.red)
	if _, err := st.Append(ctx, r.red.ID(), user("native")); err != nil {
		t.Errorf("a native entry on an imported redacted session: %v", err)
	}
}

// redactedPushGrown: a redacted session pushed, then pushed again once
// it has grown, lands what it added, as any session's push does.
func redactedPushGrown(t *testing.T, opts Options) {
	hooks(t, opts.Push != nil)
	st := opts.New(t)
	orig := original(t, "orig-push")
	r := newRedactor(orig, "test.secret@1", agentsession.Header{})
	r.redact(t, orig)
	if err := opts.Push(t, st, fileOf(t, r.red)); err != nil {
		t.Fatalf("push of a redacted session: %v", err)
	}
	sameRedacted(t, st, r.red)
	if _, err := orig.Append(user("more, " + secret)); err != nil {
		t.Fatal(err)
	}
	r.redact(t, orig)
	if err := opts.Push(t, st, fileOf(t, r.red)); err != nil {
		t.Fatalf("push of the grown redacted session: %v", err)
	}
	sameRedacted(t, st, r.red)
}

// redactedOnly: a store set to hold only redacted entries refuses a
// header without redacted and every entry with content without it, by create,
// append, import and push: an original, a fork, and a redacted session
// resumed with a native entry. It takes a redacted session created,
// grown, imported and pushed, as long as every entry is redacted.
func redactedOnly(t *testing.T, opts Options) {
	hooks(t, opts.RedactedOnly != nil)
	ctx := context.Background()
	st := opts.RedactedOnly(t)
	orig := original(t, "orig-only")

	_, err := st.Create(ctx, agentsession.Header{})
	wantErr(t, "Create of an original", err, agentsession.ErrNotRedacted)
	r := newRedactor(orig, "test.created@1", agentsession.Header{})
	if _, err := st.Create(ctx, r.red.Header()); err != nil {
		t.Fatalf("Create of a redacted session: %v", err)
	}
	appendAll(t, st, r.red.ID(), r.redact(t, orig))
	_, err = st.Append(ctx, r.red.ID(), user("native"))
	wantErr(t, "a native append", err, agentsession.ErrNotRedacted)
	// A leaf label records a head move and carries no content to have
	// been redacted, so the store takes it (RFC 0002, Redacted sessions).
	if _, err = st.Append(ctx, r.red.ID(), agentsession.NewLabelEntry(r.red.Leaf(), agentsession.LeafLabel)); err != nil {
		t.Errorf("a native leaf label: %v, want it taken", err)
	}
	sameRedacted(t, st, r.red)
	// A fork of it: not redacted, refused; redacted, its prefix all
	// redacted, taken, and refused a native entry.
	_, err = st.Create(ctx, agentsession.Header{Base: r.red.Leaf(), ParentSession: r.red.ID()})
	wantErr(t, "Create of a fork without redacted", err, agentsession.ErrNotRedacted)
	rf, err := st.Create(ctx, agentsession.Header{ID: "fork-redacted", Base: r.red.Leaf(), ParentSession: r.red.ID(), Redacted: true,
		RedactedFrom: &agentsession.RedactedFrom{Session: "orig-only-fork", Profile: "test.fork@1"}})
	if err != nil {
		t.Fatalf("Create of a redacted fork whose prefix is redacted: %v", err)
	}
	_, err = st.Append(ctx, rf.ID(), user("native on the fork"))
	wantErr(t, "a native append to the fork", err, agentsession.ErrNotRedacted)

	resumed := newRedactor(orig, "test.resumed@1", agentsession.Header{})
	resumed.redact(t, orig)
	if _, err := resumed.red.Append(user("resumed natively")); err != nil {
		t.Fatal(err)
	}
	fork, err := agentsession.Fork(r.red, r.red.Leaf(), agentsession.Header{ID: "fork-of-redacted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(user("unredacted " + secret)); err != nil {
		t.Fatal(err)
	}
	if take := importer(opts, st); take != nil {
		wantErr(t, "import of an original", take(t, st, fileOf(t, orig)), agentsession.ErrNotRedacted)
		wantErr(t, "import of a resumed redacted session", take(t, st, fileOf(t, resumed.red)), agentsession.ErrNotRedacted)
		wantErr(t, "import of a fork of a redacted session", take(t, st, fileOf(t, fork)), agentsession.ErrNotRedacted)
		imported := newRedactor(orig, "test.imported@1", agentsession.Header{})
		imported.redact(t, orig)
		if err := take(t, st, fileOf(t, imported.red)); err != nil {
			t.Fatalf("import of a redacted session: %v", err)
		}
		sameRedacted(t, st, imported.red)
	}
	if opts.Push != nil {
		wantErr(t, "push of an original", opts.Push(t, st, fileOf(t, orig)), agentsession.ErrNotRedacted)
		wantErr(t, "push of a resumed redacted session", opts.Push(t, st, fileOf(t, resumed.red)), agentsession.ErrNotRedacted)
		wantErr(t, "push of a fork of a redacted session", opts.Push(t, st, fileOf(t, fork)), agentsession.ErrNotRedacted)
		pushed := newRedactor(orig, "test.pushed@1", agentsession.Header{})
		pushed.redact(t, orig)
		if err := opts.Push(t, st, fileOf(t, pushed.red)); err != nil {
			t.Fatalf("push of a redacted session: %v", err)
		}
		if _, err := orig.Append(user("more, " + secret)); err != nil {
			t.Fatal(err)
		}
		pushed.redact(t, orig)
		if err := opts.Push(t, st, fileOf(t, pushed.red)); err != nil {
			t.Fatalf("push of the grown redacted session: %v", err)
		}
		sameRedacted(t, st, pushed.red)
		if _, err := pushed.red.Append(user("resumed natively")); err != nil {
			t.Fatal(err)
		}
		wantErr(t, "push of the session resumed natively", opts.Push(t, st, fileOf(t, pushed.red)), agentsession.ErrNotRedacted)
	}
	// An entry whose redacted member is not a marker: one from before
	// 0.13, which that minor does not define, and one in a form no minor
	// defines. Neither is redacted, by import, append or push.
	oldFile, oldRead := oldRedacted(t, "old-only")
	badFile := malformedRedacted(t, orig, "test.malformed@1")
	// A remote client's own Prepare refuses this entry first, as one it
	// may not append anywhere (ErrNoRaise); a store refuses it as not
	// redacted.
	_, err = st.Append(ctx, r.red.ID(), oldRead.Entries()[0])
	if !errors.Is(err, agentsession.ErrNotRedacted) && !errors.Is(err, agentsession.ErrNoRaise) {
		t.Errorf("an append of an entry read with a redacted member from before 0.13: %v, want ErrNotRedacted", err)
	}
	bad := agentsession.NewItemEntry(openresponses.UserText("[bad]"))
	bad.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(`[[2,1]]`)}
	_, err = st.Append(ctx, r.red.ID(), bad)
	wantErr(t, "an append of an entry with a malformed redacted", err, agentsession.ErrNotRedacted)
	if take := importer(opts, st); take != nil {
		wantErr(t, "import of a 0.12 file with redacted members", take(t, st, oldFile), agentsession.ErrNotRedacted)
		wantErr(t, "import of a file with a malformed redacted", take(t, st, badFile), agentsession.ErrNotRedacted)
	}
	if opts.Push != nil {
		wantErr(t, "push of a 0.12 session with redacted members", opts.Push(t, st, oldFile), agentsession.ErrNotRedacted)
		wantErr(t, "push of a session with a malformed redacted", opts.Push(t, st, badFile), agentsession.ErrNotRedacted)
	}
	for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if !sum.Header.Redacted {
			t.Errorf("the store holds %s, which is not redacted", sum.Header.ID)
			continue
		}
		for _, e := range readBack(t, st, sum.Header.ID).Entries() {
			if _, ok := agentsession.RedactedOf(e); !ok && !agentsession.HeadMove(e) {
				t.Errorf("the store holds %s in %s, which is not redacted", e.Base().ID, sum.Header.ID)
			}
		}
	}
}

// redactedOriginal: Original finds a redacted session's original in the
// store and maps each redacted entry to the entries its positions name,
// prefix included, ranges expanded, however the original has grown
// since; a native entry and an added one map to nothing; a position
// past the original the store holds is ErrStaleOriginal; and an
// original the store lacks, or a session that is not redacted, is
// refused.
func redactedOriginal(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	origin, err := st.Create(ctx, agentsession.Header{ID: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	var base string
	for _, text := range []string{"start", "base " + secret} {
		if base, err = st.Append(ctx, origin.ID(), user(text)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "orig", ParentSession: origin.ID(), Base: base}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"own " + secret, "and more"} {
		if _, err := st.Append(ctx, "orig", user(text)); err != nil {
			t.Fatal(err)
		}
	}
	orig := readBack(t, st, "orig")
	order := agentsession.FileOrder(orig)
	if len(order) != 4 || !orig.Prefix(order[0].Base().ID) || orig.Prefix(order[3].Base().ID) {
		t.Fatalf("the original in file order: %d entries", len(order))
	}
	// A redaction written without a base: the original's prefix is
	// redacted into entries of its own, whose positions count in it.
	r := newRedactor(orig, "test.secret@1", agentsession.Header{})
	r.redact(t, orig)
	// A placeholder for the last two, an added entry and a native one.
	block := agentsession.NewItemEntry(openresponses.AssistantText("[two entries redacted]"))
	if err := agentsession.SetRedacted(block, []agentsession.Span{{From: 3, To: 4}}); err != nil {
		t.Fatal(err)
	}
	added := agentsession.NewItemEntry(openresponses.AssistantText("[note]"))
	if err := agentsession.SetRedacted(added, nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range []agentsession.Entry{block, added} {
		if _, err := r.red.Commit(e); err != nil {
			t.Fatal(err)
		}
	}
	native, err := r.red.Append(user("native"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "orig", user("after the redaction")); err != nil {
		t.Fatal(err)
	}
	got, m, err := agentsession.Original(ctx, st, r.red)
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	if got.ID() != "orig" {
		t.Errorf("Original is %s, want orig", got.ID())
	}
	want := map[string][]string{}
	for i, e := range agentsession.FileOrder(r.red)[:4] {
		want[e.Base().ID] = []string{order[i].Base().ID}
	}
	want[block.Base().ID] = []string{order[2].Base().ID, order[3].Base().ID}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("the mapping %v, want %v", m, want)
	}
	if _, in := m[added.Base().ID]; in {
		t.Error("an added entry is mapped")
	}
	if _, in := m[native]; in {
		t.Error("a native entry is mapped")
	}

	beyond := newRedactor(orig, "test.beyond@1", agentsession.Header{})
	far := user("[far]")
	if err := agentsession.SetRedacted(far, []agentsession.Span{{From: 9, To: 9}}); err != nil {
		t.Fatal(err)
	}
	if _, err := beyond.red.Commit(far); err != nil {
		t.Fatal(err)
	}
	_, _, err = agentsession.Original(ctx, st, beyond.red)
	wantErr(t, "a position past the original held", err, agentsession.ErrStaleOriginal)

	gone, _ := redaction(t, original(t, "not-held"), "test.secret@1")
	_, _, err = agentsession.Original(ctx, st, gone)
	wantErr(t, "Original whose original the store lacks", err, agentsession.ErrNoSession)
	_, _, err = agentsession.Original(ctx, st, orig)
	wantErr(t, "Original of a session that is not redacted", err, agentsession.ErrNotRedacted)
}

// redactedOriginalFork: a redacted session that is a fork of another
// redacted session shares its prefix, whose positions count in the
// original of the session parent_session names: Original maps the
// fork's own entries in its own original and the prefix in that one,
// through the redacted session the store holds. O is an original and RO
// its redaction; F is a fork of O, and RF its redaction, made as a fork
// of RO.
func redactedOriginalFork(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	o := original(t, "fo-o")
	ro := newRedactor(o, "test.secret@1", agentsession.Header{})
	ro.redact(t, o)
	base := o.Entries()[1].Base().ID // the assistant's reply
	f, err := agentsession.Fork(o, base, agentsession.Header{ID: "fo-f"})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"forked " + secret, "on"} {
		if _, err := f.Append(user(text)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []*agentsession.Session{o, ro.red, f} {
		if err := take(t, st, fileOf(t, s)); err != nil {
			t.Fatalf("import %s: %v", s.ID(), err)
		}
	}
	roBase := ro.ids[base]
	rf, err := agentsession.Fork(ro.red, roBase, agentsession.Header{ID: "fo-rf", ParentSession: ro.red.ID(), Redacted: true,
		RedactedFrom: &agentsession.RedactedFrom{Session: "fo-f", Profile: "test.secret@1"}})
	if err != nil {
		t.Fatal(err)
	}
	forder := agentsession.FileOrder(f)
	var own []string
	for i, e := range forder {
		if f.Prefix(e.Base().ID) {
			continue
		}
		n := user(fmt.Sprintf("[redacted %d]", i+1))
		if err := agentsession.SetRedacted(n, []agentsession.Span{{From: i + 1, To: i + 1}}); err != nil {
			t.Fatal(err)
		}
		res, err := rf.Commit(n)
		if err != nil {
			t.Fatal(err)
		}
		own = append(own, res.ID)
	}
	if err := take(t, st, fileOf(t, rf)); err != nil {
		t.Fatalf("import the redacted fork: %v", err)
	}
	held := readBack(t, st, "fo-rf")
	got, m, err := agentsession.Original(ctx, st, held)
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	if got.ID() != "fo-f" {
		t.Errorf("Original is %s, want fo-f", got.ID())
	}
	want := map[string][]string{}
	oOrder := agentsession.FileOrder(o)
	for _, e := range agentsession.FileOrder(held) {
		if !held.Prefix(e.Base().ID) {
			continue
		}
		spans, _ := agentsession.RedactedOf(e)
		want[e.Base().ID] = []string{oOrder[spans[0].From-1].Base().ID}
	}
	prefixLen := len(want)
	for i, id := range own {
		want[id] = []string{forder[prefixLen+i].Base().ID}
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("the mapping %v, want %v", m, want)
	}

	// Two hops: RF2, a fork of RF at the same base, whose parent holds
	// the prefix only on its own prefix, so the walk goes on to RO.
	f2, err := agentsession.Fork(o, base, agentsession.Header{ID: "fo-f2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Append(user("second fork")); err != nil {
		t.Fatal(err)
	}
	if err := take(t, st, fileOf(t, f2)); err != nil {
		t.Fatal(err)
	}
	rf2, err := agentsession.Fork(held, roBase, agentsession.Header{ID: "fo-rf2", ParentSession: "fo-rf", Redacted: true,
		RedactedFrom: &agentsession.RedactedFrom{Session: "fo-f2", Profile: "test.secret@1"}})
	if err != nil {
		t.Fatal(err)
	}
	f2order := agentsession.FileOrder(f2)
	own2 := user("[redacted second]")
	if err := agentsession.SetRedacted(own2, []agentsession.Span{{From: len(f2order), To: len(f2order)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rf2.Commit(own2); err != nil {
		t.Fatal(err)
	}
	if err := take(t, st, fileOf(t, rf2)); err != nil {
		t.Fatalf("import the second redacted fork: %v", err)
	}
	held2 := readBack(t, st, "fo-rf2")
	_, m2, err := agentsession.Original(ctx, st, held2)
	if err != nil {
		t.Fatalf("Original two hops away: %v", err)
	}
	want2 := map[string][]string{own2.Base().ID: {f2order[len(f2order)-1].Base().ID}}
	for _, e := range agentsession.FileOrder(held2) {
		if held2.Prefix(e.Base().ID) {
			want2[e.Base().ID] = want[e.Base().ID]
		}
	}
	if !reflect.DeepEqual(m2, want2) {
		t.Errorf("the mapping two hops away %v, want %v", m2, want2)
	}

	// An ancestor's original held short: the prefix's positions name
	// entries past it, which is ErrStaleOriginal, as for s's own.
	short := opts.New(t)
	takeShort := importer(opts, short)
	ofile := fileOf(t, o)
	lines := strings.SplitAfter(string(ofile), "\n")
	for _, file := range [][]byte{[]byte(lines[0] + lines[1]), fileOf(t, ro.red), fileOf(t, f), fileOf(t, rf)} {
		if err := takeShort(t, short, file); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = agentsession.Original(ctx, short, readBack(t, short, "fo-rf"))
	wantErr(t, "Original whose ancestor's original is held short", err, agentsession.ErrStaleOriginal)
}

// redactedRedactions: Redactions lists the redacted sessions the store
// holds that name the original, and no other.
func redactedRedactions(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	orig, other := original(t, "orig-listed"), original(t, "orig-other")
	if err := take(t, st, fileOf(t, orig)); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, profile := range []string{"test.secret@1", "test.secret@2"} {
		red, file := redaction(t, orig, profile)
		if err := take(t, st, file); err != nil {
			t.Fatal(err)
		}
		want[red.ID()] = true
	}
	_, file := redaction(t, other, "test.secret@1")
	if err := take(t, st, file); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for sum, err := range agentsession.Redactions(ctx, st, orig.ID()) {
		if err != nil {
			t.Fatal(err)
		}
		got[sum.Header.ID] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Redactions of %s: %v, want %v", orig.ID(), got, want)
	}
}

// redactedForkRaise: a fork is written at this package's format, so one
// whose prefix holds a redacted member from before 0.13, which that
// minor would give a meaning it was not written with, is refused, as
// RFC 0001's no-raise rule has it; a fork above it is not.
func redactedForkRaise(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	old := agentsession.New(agentsession.Header{ID: "old-redacted", Format: "agentsession/0.12"})
	first, err := old.Commit(user("before"))
	if err != nil {
		t.Fatal(err)
	}
	marked := agentsession.NewItemEntry(openresponses.UserText("an old redacted member"))
	marked.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(`[1]`)}
	gated, err := old.Commit(marked)
	if err != nil {
		t.Fatal(err)
	}
	if err := take(t, st, fileOf(t, old)); err != nil {
		t.Fatalf("import of a 0.12 session holding a redacted member: %v", err)
	}
	_, err = st.Create(ctx, agentsession.Header{Base: gated.ID, ParentSession: "old-redacted"})
	wantErr(t, "a fork whose prefix holds a redacted member from before 0.13", err, agentsession.ErrUnsupportedFormat)
	if _, err := st.Create(ctx, agentsession.Header{ID: "above-it", Base: first.ID, ParentSession: "old-redacted"}); err != nil {
		t.Errorf("a fork above the old member: %v", err)
	}

	// The base held only on a prefix: F is a 0.12 fork of an origin the
	// store does not hold, and its prefix holds the old member.
	o := agentsession.New(agentsession.Header{ID: "absent-origin", Format: "agentsession/0.12"})
	if _, err := o.Commit(user("before")); err != nil {
		t.Fatal(err)
	}
	g := agentsession.NewItemEntry(openresponses.UserText("an old redacted member"))
	g.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(`[1]`)}
	gr, err := o.Commit(g)
	if err != nil {
		t.Fatal(err)
	}
	f, err := agentsession.Fork(o, gr.ID, agentsession.Header{ID: "held-fork", Format: "agentsession/0.12"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Commit(user("on the fork")); err != nil {
		t.Fatal(err)
	}
	if err := take(t, st, fileOf(t, f)); err != nil {
		t.Fatalf("import of a 0.12 fork without its origin: %v", err)
	}
	_, err = st.Create(ctx, agentsession.Header{Base: gr.ID, ParentSession: "held-fork"})
	wantErr(t, "a fork at an old member held only on a prefix", err, agentsession.ErrUnsupportedFormat)

	// The other way round: a fork written under 0.12 over a prefix of
	// redacted entries, which that minor would read as not redacted.
	r := newRedactor(original(t, "orig-raise"), "test.secret@1", agentsession.Header{})
	if _, err := st.Create(ctx, r.red.Header()); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, r.red.ID(), r.redact(t, original(t, "orig-raise")))
	_, err = st.Create(ctx, agentsession.Header{Format: "agentsession/0.12", Base: r.red.Leaf(), ParentSession: r.red.ID()})
	wantErr(t, "a fork under 0.12 over a redacted prefix", err, agentsession.ErrUnsupportedFormat)
}

// oldRedacted is a 0.12 redacted session whose entries carry redacted
// members, which that minor does not define, so none is redacted; its
// file and the session as Read gives it.
func oldRedacted(t *testing.T, id string) ([]byte, *agentsession.Session) {
	t.Helper()
	old := agentsession.New(agentsession.Header{ID: id, Format: "agentsession/0.12", Redacted: true})
	for i, text := range []string{"[one]", "[two]"} {
		e := agentsession.NewItemEntry(openresponses.UserText(text))
		e.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(fmt.Sprintf("[%d]", i+1))}
		if _, err := old.Commit(e); err != nil {
			t.Fatal(err)
		}
	}
	file := fileOf(t, old)
	read, err := agentsession.Read(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range read.Entries() {
		if _, ok := agentsession.RedactedOf(e); ok {
			t.Fatalf("a 0.12 redacted member reads as a marker: %s", e.Base().ID)
		}
	}
	return file, read
}

// malformedRedacted is a redaction of orig under profile, one of whose
// entries carries a redacted member in a form no minor defines.
func malformedRedacted(t *testing.T, orig *agentsession.Session, profile string) []byte {
	t.Helper()
	r := newRedactor(orig, profile, agentsession.Header{})
	r.redact(t, orig)
	bad := agentsession.NewItemEntry(openresponses.UserText("[bad]"))
	bad.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(`[0]`)}
	if _, err := r.red.Commit(bad); err != nil {
		t.Fatal(err)
	}
	return fileOf(t, r.red)
}

// redactedCreateMalformed: a create of a header whose redacted_from RFC
// 0001 calls malformed, naming the session itself, with its format
// given or for the store to fill, is refused with ErrRedactedFrom in
// every store, and leaves nothing; a store would otherwise hold a
// session it could not read back.
func redactedCreateMalformed(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	for _, h := range []agentsession.Header{
		{ID: "r-itself", Redacted: true, RedactedFrom: &agentsession.RedactedFrom{Session: "r-itself"}},
		{ID: "r-itself-later", Format: agentsession.Format, Redacted: true, RedactedFrom: &agentsession.RedactedFrom{Session: "r-itself-later", Profile: "p"}},
	} {
		_, err := st.Create(ctx, h)
		wantErr(t, "Create of "+h.ID, err, agentsession.ErrRedactedFrom)
		if _, err := st.Open(ctx, h.ID); !errors.Is(err, agentsession.ErrNoSession) {
			t.Errorf("the refused create left %s: %v", h.ID, err)
		}
	}
}
