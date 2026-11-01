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
