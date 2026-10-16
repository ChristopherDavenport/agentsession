package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testRedacted exercises RFC 0002's redacted sessions: no store creates
// one or appends to one; a store that takes a session whole, by import
// or by push, holds one and verifies it, and refuses a push that would
// add to one it holds; a store set to hold only redacted sessions
// refuses every other by every path it has; and [agentsession.Original]
// and [agentsession.Redactions] find what was made from what.
func testRedacted(t *testing.T, opts Options) {
	t.Run("CreateRefused", func(t *testing.T) { redactedCreate(t, opts) })
	t.Run("Import", func(t *testing.T) { redactedImport(t, opts) })
	t.Run("AppendRefused", func(t *testing.T) { redactedAppend(t, opts) })
	t.Run("Push", func(t *testing.T) { redactedPush(t, opts) })
	t.Run("PushAddsRefused", func(t *testing.T) { redactedPushAdds(t, opts) })
	t.Run("ContinueRefused", func(t *testing.T) { redactedContinue(t, opts) })
	t.Run("RedactedOnly", func(t *testing.T) { redactedOnly(t, opts) })
	t.Run("Original", func(t *testing.T) { redactedOriginal(t, opts) })
	t.Run("Redactions", func(t *testing.T) { redactedRedactions(t, opts) })
}

// secret is what the redactions here take out.
const secret = "hunter2"

func hide(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") }

// original builds, in memory, a session holding the secret on more
// than one branch, with a label naming an entry, so that a redaction
// rewrites bodies and re-points parents and targets.
func original(t *testing.T, id string) *agentsession.Session {
	t.Helper()
	s := agentsession.New(agentsession.Header{ID: id, CWD: "/w"})
	root, err := s.Append(user("my password is " + secret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(agentsession.NewItemEntry(openresponses.AssistantText("noted: " + secret))); err != nil {
		t.Fatal(err)
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

// redaction makes a redaction of orig with the exported writer helpers,
// as a redactor does: every body string through one Rewriter in file
// order, every reference into the session re-pointed with RemapRefs to
// the entries already written, each entry committed, its ID cleared, to
// a session whose header carries redacted and redacted_from under the ID
// RedactedSessionID derives, and last a fork's base. It returns the
// session as a reader of its file has it, and the file.
func redaction(t *testing.T, orig *agentsession.Session, profile string, aligned bool) (*agentsession.Session, []byte) {
	t.Helper()
	oh := orig.Header()
	order := agentsession.FileOrder(orig)
	h := oh
	h.ID = agentsession.RedactedSessionID(oh.ID, len(order), profile)
	h.Base = ""
	h.Redacted = true
	h.RedactedFrom = &agentsession.RedactedFrom{Session: oh.ID, Entries: len(order), Profile: profile, Aligned: aligned}
	red := agentsession.New(h)
	ids := map[string]string{}
	remap := agentsession.Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != oh.ID {
			return "", false
		}
		id, ok := ids[entry]
		return id, ok
	}}
	rw := agentsession.NewRewriter(hide)
	for _, e := range order {
		body, err := rw.Rewrite(e)
		if err != nil {
			t.Fatalf("rewrite %s: %v", e.Base().ID, err)
		}
		r, err := agentsession.RemapRefs(body, remap)
		if err != nil {
			t.Fatalf("remap %s: %v", e.Base().ID, err)
		}
		n := r.Entry
		n.Base().ID = ""
		res, err := red.Commit(n)
		if err != nil {
			t.Fatalf("commit %s: %v", e.Base().ID, err)
		}
		ids[e.Base().ID] = res.ID
	}
	if oh.Base != "" {
		h.Base = oh.Base
		var unmapped []agentsession.Reference
		if h, unmapped = agentsession.RemapHeaderRefs(h, agentsession.Remap{Entry: remap.Entry}); len(unmapped) > 0 {
			t.Fatalf("the base is not mapped: %v", unmapped)
		}
	}
	var head, body bytes.Buffer
	if err := agentsession.Write(&head, agentsession.New(h)); err != nil {
		t.Fatal(err)
	}
	if err := agentsession.Write(&body, red); err != nil {
		t.Fatal(err)
	}
	file := head.String() + strings.SplitAfterN(body.String(), "\n", 2)[1]
	s, err := agentsession.Read(strings.NewReader(file))
	if err != nil {
		t.Fatalf("the redacted file does not read: %v\n%s", err, file)
	}
	if strings.Contains(file, secret) {
		t.Fatalf("the redaction kept the secret:\n%s", file)
	}
	return s, []byte(file)
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

// cut is a file holding the header and the first n entry lines of file.
func cut(file []byte, n int) []byte {
	lines := strings.SplitAfter(string(file), "\n")
	return []byte(strings.Join(lines[:n+1], ""))
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

// sameRedacted checks that the store holds want whole: redacted, linked
// to its original, its entries in file order, and verifying when its
// file is read again.
func sameRedacted(t *testing.T, st agentsession.Store, want *agentsession.Session) {
	t.Helper()
	got := readBack(t, st, want.ID())
	h := got.Header()
	if !h.Redacted || !reflect.DeepEqual(h.RedactedFrom, want.Header().RedactedFrom) {
		t.Errorf("held header: redacted %v, redacted_from %+v; want %+v", h.Redacted, h.RedactedFrom, want.Header().RedactedFrom)
	}
	if g, w := entryIDs(got), entryIDs(want); !reflect.DeepEqual(g, w) {
		t.Errorf("held entries %v, want %v", g, w)
	}
	if _, err := agentsession.Read(bytes.NewReader(fileOf(t, got))); err != nil {
		t.Errorf("the held session does not verify: %v", err)
	}
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

// redactedCreate: no store creates a session whose header carries
// redacted, linked or not, or one carrying redacted_from without it,
// and nothing is left of the attempt.
func redactedCreate(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	for _, h := range []agentsession.Header{
		{ID: "r-linked", Redacted: true, RedactedFrom: &agentsession.RedactedFrom{Session: "o", Entries: 1, Profile: "test@1", Aligned: true}},
		{ID: "r-bare", Redacted: true},
		{Redacted: true},
	} {
		_, err := st.Create(ctx, h)
		wantErr(t, "Create of a redacted header "+h.ID, err, agentsession.ErrRedacted)
	}
	_, err := st.Create(ctx, agentsession.Header{ID: "r-unmarked", RedactedFrom: &agentsession.RedactedFrom{Session: "o", Entries: 1}})
	wantErr(t, "Create of a redacted_from without redacted", err, agentsession.ErrRedactedFrom)
	_, err = st.Create(ctx, agentsession.Header{ID: "r-unmarked-extra", Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`{"entries":1,"session":"o"}`)}})
	wantErr(t, "Create of a redacted_from without redacted, held in Extra", err, agentsession.ErrRedactedFrom)
	if n := countSessions(t, st); n != 0 {
		t.Errorf("the store holds %d sessions after the refusals", n)
	}
}

// redactedImport: a redacted file taken whole is held as written and
// verifies, beside the original.
func redactedImport(t *testing.T, opts Options) {
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	orig := original(t, "orig-import")
	red, file := redaction(t, orig, "test.secret@1", true)
	if err := take(t, st, fileOf(t, orig)); err != nil {
		t.Fatalf("import the original: %v", err)
	}
	if err := take(t, st, file); err != nil {
		t.Fatalf("import the redaction: %v", err)
	}
	sameRedacted(t, st, red)
}

// redactedAppend: a held redacted session takes no append, an entry or
// a leaf label, and is as it was, read again and after a reopen.
func redactedAppend(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	red, file := redaction(t, original(t, "orig-append"), "test.secret@1", true)
	if err := take(t, st, file); err != nil {
		t.Fatal(err)
	}
	s, err := st.Open(ctx, red.ID())
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Append(ctx, red.ID(), user("more"))
	wantErr(t, "Append to a redacted session", err, agentsession.ErrRedacted)
	_, err = st.Append(ctx, red.ID(), agentsession.NewLabelEntry(red.Leaf(), agentsession.LeafLabel))
	wantErr(t, "a leaf label on a redacted session", err, agentsession.ErrRedacted)
	if w, ok := st.(agentsession.Writer); ok {
		_, err := w.Write(ctx, red.ID(), user("more"))
		wantErr(t, "Write to a redacted session", err, agentsession.ErrRedacted)
	}
	if s.Len() != red.Len() {
		t.Errorf("the session holds %d entries after the refusals, want %d", s.Len(), red.Len())
	}
	sameRedacted(t, st, red)
	if opts.Reopen != nil {
		sameRedacted(t, opts.Reopen(t, st), red)
	}
}

// redactedPush: a store that lacks a redacted session takes it whole by
// push, and a repeat of the push, which adds nothing, is taken too.
func redactedPush(t *testing.T, opts Options) {
	hooks(t, opts.Push != nil)
	st := opts.New(t)
	red, file := redaction(t, original(t, "orig-push"), "test.secret@1", true)
	if err := opts.Push(t, st, file); err != nil {
		t.Fatalf("push of a redacted session: %v", err)
	}
	sameRedacted(t, st, red)
	if err := opts.Push(t, st, file); err != nil {
		t.Errorf("a repeat of the push: %v", err)
	}
	sameRedacted(t, st, red)
}

// redactedPushAdds: a push that would add entries to a held redacted
// session is refused, and lands nothing.
func redactedPushAdds(t *testing.T, opts Options) {
	hooks(t, opts.Push != nil)
	st := opts.New(t)
	red, file := redaction(t, original(t, "orig-adds"), "test.secret@1", true)
	part := cut(file, 2)
	held, err := agentsession.Read(bytes.NewReader(part))
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.Push(t, st, part); err != nil {
		t.Fatalf("push of a redacted session: %v", err)
	}
	wantErr(t, "a push adding to a held redacted session", opts.Push(t, st, file), agentsession.ErrRedacted)
	if got := readBack(t, st, red.ID()); got.Len() != held.Len() {
		t.Errorf("the refused push left %d entries, want %d", got.Len(), held.Len())
	}
}

// redactedOnly: a store set to hold only redacted sessions refuses an
// original and a fork of a redacted session by every path it has, a
// create, an import and a push, and takes a redacted session by the
// paths that take one.
func redactedOnly(t *testing.T, opts Options) {
	hooks(t, opts.RedactedOnly != nil)
	ctx := context.Background()
	st := opts.RedactedOnly(t)
	orig := original(t, "orig-only")
	red, file := redaction(t, orig, "test.secret@1", true)
	origFile := fileOf(t, orig)

	_, err := st.Create(ctx, agentsession.Header{})
	wantErr(t, "Create of an original", err, agentsession.ErrNotRedacted)
	_, err = st.Create(ctx, agentsession.Header{Redacted: true})
	wantErr(t, "Create of a redacted header", err, agentsession.ErrRedacted)

	held := false
	take := importer(opts, st)
	if take != nil {
		wantErr(t, "import of an original", take(t, st, origFile), agentsession.ErrNotRedacted)
		if err := take(t, st, file); err != nil {
			t.Fatalf("import of a redacted session: %v", err)
		}
		held = true
	}
	if opts.Push != nil {
		wantErr(t, "push of an original", opts.Push(t, st, origFile), agentsession.ErrNotRedacted)
		if err := opts.Push(t, st, file); err != nil {
			t.Fatalf("push of a redacted session: %v", err)
		}
		held = true
	}
	if held {
		sameRedacted(t, st, red)
		// A fork of the redacted session is not redacted: refused by
		// create, by import and by push.
		_, err := st.Create(ctx, agentsession.Header{Base: red.Leaf(), ParentSession: red.ID()})
		wantErr(t, "Create of a fork of a redacted session", err, agentsession.ErrNotRedacted)
		fork, err := agentsession.Fork(red, red.Leaf(), agentsession.Header{ID: "fork-of-redacted"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fork.Append(user("unredacted " + secret)); err != nil {
			t.Fatal(err)
		}
		forkFile := fileOf(t, fork)
		if take != nil {
			wantErr(t, "import of a fork of a redacted session", take(t, st, forkFile), agentsession.ErrNotRedacted)
		}
		if opts.Push != nil {
			wantErr(t, "push of a fork of a redacted session", opts.Push(t, st, forkFile), agentsession.ErrNotRedacted)
		}
	}
	for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if !sum.Header.Redacted {
			t.Errorf("the store holds %s, which is not redacted", sum.Header.ID)
		}
	}
}

// redactedOriginal: Original finds a redacted session's original in the
// store, with the entries mapped by position in file order, the prefix
// included, over the entries the original held when it was redacted,
// however it has grown since; a redaction that is not aligned gets no
// mapping; and an original the store lacks, or a session that is not
// redacted, is refused.
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
	red, _ := redaction(t, orig, "test.secret@1", true)
	if _, err := st.Append(ctx, "orig", user("after the redaction")); err != nil {
		t.Fatal(err)
	}
	got, m, err := agentsession.Original(ctx, st, red)
	if err != nil {
		t.Fatalf("Original: %v", err)
	}
	if got.ID() != "orig" {
		t.Errorf("Original is %s, want orig", got.ID())
	}
	want := map[string]string{}
	for i, e := range agentsession.FileOrder(red) {
		want[e.Base().ID] = order[i].Base().ID
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("the mapping %v, want %v", m, want)
	}

	loose, _ := redaction(t, orig, "test.loose@1", false)
	if got, m, err := agentsession.Original(ctx, st, loose); err != nil || got.ID() != "orig" || m != nil {
		t.Errorf("Original of a redaction that is not aligned: %v, mapping %v", err, m)
	}

	gone, _ := redaction(t, original(t, "not-held"), "test.secret@1", true)
	_, _, err = agentsession.Original(ctx, st, gone)
	wantErr(t, "Original whose original the store lacks", err, agentsession.ErrNoSession)
	_, _, err = agentsession.Original(ctx, st, orig)
	wantErr(t, "Original of a session that is not redacted", err, agentsession.ErrNotRedacted)
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
		red, file := redaction(t, orig, profile, true)
		if err := take(t, st, file); err != nil {
			t.Fatal(err)
		}
		want[red.ID()] = true
	}
	_, file := redaction(t, other, "test.secret@1", true)
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

// redactedContinue: Continue and ContinueRef refuse a redacted session
// before they create anything, since it takes no continued_in link, and
// a ref naming it stays where it was.
func redactedContinue(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)
	red, file := redaction(t, original(t, "orig-continue"), "test.secret@1", true)
	if err := take(t, st, file); err != nil {
		t.Fatal(err)
	}
	before := countSessions(t, st)
	_, err := agentsession.Continue(ctx, st, red.ID(), nil)
	wantErr(t, "Continue of a redacted session", err, agentsession.ErrRedacted)
	if rs, ok := st.(agentsession.RefStore); ok {
		if err := rs.UpdateRef(ctx, "redacted/test", agentsession.RefTarget{}, agentsession.RefTarget{Session: red.ID()}, "test"); err != nil {
			t.Fatal(err)
		}
		_, err := agentsession.ContinueRef(ctx, st, "redacted/test", nil)
		wantErr(t, "ContinueRef of a redacted session", err, agentsession.ErrRedacted)
		if got, err := rs.ResolveRef(ctx, "redacted/test"); err != nil || got.Session != red.ID() {
			t.Errorf("after the refusal the ref names %+v (%v)", got, err)
		}
	}
	if n := countSessions(t, st); n != before {
		t.Errorf("the refusals left %d sessions, %d before", n, before)
	}
	sameRedacted(t, st, red)
}
