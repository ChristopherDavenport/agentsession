package storetest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agentsession"
)

// testBlobs holds a store to RFC 0002's Media rule: a store that holds
// sidecar sessions accepts a blob by its bytes and stores it under its
// hash, and one that cannot refuses to create a sidecar session. A
// store with an import takes a sidecar session with its directory: each
// blob verified and stored, one the directory lacks reported
// unresolved, and one that fails its name refusing the import, which
// leaves nothing.
func testBlobs(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	bw, ok := st.(agentsession.BlobWriter)
	if !ok {
		_, err := st.Create(ctx, agentsession.Header{ID: "side", Media: agentsession.MediaSidecar})
		wantErr(t, "a sidecar session in a store that holds no blobs", err, agentsession.ErrNoBlobWriter)
		return
	}
	data := []byte("image bytes")
	h, err := bw.PutBlob(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if want := agentsession.HashBytes(data); h != want {
		t.Errorf("PutBlob: %s, want %s", h, want)
	}
	if again, err := bw.PutBlob(ctx, data); err != nil || again != h {
		t.Errorf("PutBlob again: %s, %v", again, err)
	}
	br, _ := st.(agentsession.BlobReader)
	if br != nil {
		if got, err := br.Blob(ctx, h); err != nil || !bytes.Equal(got, data) {
			t.Errorf("Blob: %q, %v", got, err)
		}
		_, err := br.Blob(ctx, agentsession.HashBytes([]byte("never stored")))
		wantErr(t, "Blob of one never stored", err, agentsession.ErrNoBlob)
	}

	im, ok := st.(agentsession.Importer)
	if !ok {
		return
	}
	held, lacked := []byte("held in the directory"), []byte("in no directory")
	hh, hl := agentsession.HashBytes(held), agentsession.HashBytes(lacked)
	sess := agentsession.New(agentsession.Header{ID: "side-imp", Media: agentsession.MediaSidecar})
	for _, b := range []string{hh, hl} {
		if _, err := sess.Append(user("see sidecar:" + b)); err != nil {
			t.Fatal(err)
		}
	}
	file := fileOf(t, sess)
	hex := func(h string) string { return strings.TrimPrefix(h, agentsession.HashPrefix) }

	bad := fstest.MapFS{hex(hh): {Data: []byte("other bytes")}}
	_, _, err = agentsession.ImportMedia(ctx, im, bytes.NewReader(file), bad, true)
	wantErr(t, "an import whose blob fails its name", err, agentsession.ErrBlobMismatch)
	if _, err := st.Open(ctx, "side-imp"); err == nil {
		t.Error("a refused import left its session")
	}

	// A blob the directory lacks is reported unresolved and the session
	// imported, by a store that takes the file itself; over RFC 0003 the
	// import is refused with ErrNoBlob, nothing imported, and the caller
	// uploads the blob and imports again.
	dir := fstest.MapFS{hex(hh): {Data: held}}
	got, unresolved, err := agentsession.ImportMedia(ctx, im, bytes.NewReader(file), dir, true)
	if errors.Is(err, agentsession.ErrNoBlob) {
		if _, err := st.Open(ctx, "side-imp"); err == nil {
			t.Error("an import refused for a missing blob left its session")
		}
		dir[hex(hl)] = &fstest.MapFile{Data: lacked}
		got, unresolved, err = agentsession.ImportMedia(ctx, im, bytes.NewReader(file), dir, true)
		if err == nil && len(unresolved) != 0 {
			t.Errorf("an import with every blob reports %v unresolved", unresolved)
		}
		unresolved = []string{hl}
	}
	if err != nil {
		t.Fatalf("an import with its directory: %v", err)
	}
	if got.Len() != 2 || len(unresolved) != 1 || unresolved[0] != hl {
		t.Errorf("imported %d entries, unresolved %v; want 2, [%s]", got.Len(), unresolved, hl)
	}
	if br != nil {
		if b, err := br.Blob(ctx, hh); err != nil || !bytes.Equal(b, held) {
			t.Errorf("the imported blob: %q, %v", b, err)
		}
	}
}
