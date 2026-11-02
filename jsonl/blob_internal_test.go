package jsonl

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestWritesNamingABlobFreshenIt: an append and an import that name a
// blob restart its grace before they land, so a sweep that read the
// sessions before the write, found the blob old and unnamed, and looks
// again under the media guard, spares it.
func TestWritesNamingABlobFreshenIt(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	past := time.Now().Add(-time.Hour)
	aged := func(text string) string {
		t.Helper()
		h, err := st.PutBlob(ctx, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(st.blobPath(h), past, past); err != nil {
			t.Fatal(err)
		}
		return h
	}
	fresh := func(what, h string) {
		t.Helper()
		info, err := os.Stat(st.blobPath(h))
		if err != nil || !info.ModTime().After(past.Add(time.Minute)) {
			t.Errorf("%s did not restart the grace of the blob it names: %v %v", what, info.ModTime(), err)
		}
	}
	appended := aged("named by an append")
	if _, err := st.Create(ctx, agentsession.Header{ID: "s", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "s", agentsession.NewItemEntry(openresponses.UserText("see sidecar:"+appended))); err != nil {
		t.Fatal(err)
	}
	fresh("an append", appended)

	imported := aged("named by an import")
	sess := agentsession.New(agentsession.Header{ID: "imp", Media: agentsession.MediaSidecar})
	if _, err := sess.Append(agentsession.NewItemEntry(openresponses.UserText("see sidecar:" + imported))); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Import(ctx, &buf, true); err != nil {
		t.Fatal(err)
	}
	fresh("an import", imported)
}

// TestSweepTakesStrayTypes: a type file whose blob the media directory
// does not hold, as a crash between a sweep's removing the type and the
// blob would leave the other way round, goes once older than the grace,
// so the blob's bytes stored again take the type they are then given;
// the type of a blob a session names stays.
func TestSweepTakesStrayTypes(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kept, err := st.PutTypedBlob(ctx, []byte("named"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "s", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "s", agentsession.NewItemEntry(openresponses.UserText("see sidecar:"+kept))); err != nil {
		t.Fatal(err)
	}
	stray, err := st.PutTypedBlob(ctx, []byte("lost its blob"), "text/x-stale")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(st.blobPath(stray)); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	for _, h := range []string{kept, stray} {
		for _, p := range []string{st.blobPath(h), st.typePath(h)} {
			os.Chtimes(p, past, past)
		}
	}
	if _, err := st.SweepBlobs(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.typePath(stray)); !os.IsNotExist(err) {
		t.Errorf("a type whose blob is gone survived the sweep: %v", err)
	}
	if _, mt, err := st.TypedBlob(ctx, kept); err != nil || mt != "image/png" {
		t.Errorf("the named blob after the sweep: type %q, %v", mt, err)
	}
	if _, err := st.PutTypedBlob(ctx, []byte("lost its blob"), "text/x-fresh"); err != nil {
		t.Fatal(err)
	}
	if _, mt, err := st.TypedBlob(ctx, stray); err != nil || mt != "text/x-fresh" {
		t.Errorf("bytes stored again after their stray type went: type %q, %v", mt, err)
	}
}
