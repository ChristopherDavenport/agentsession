package storetest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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

// testBlobSweep holds a store that sweeps its blobs to RFC 0002's
// retention: a blob no session names goes once older than the grace, a
// young one stays for the write that will name it, and a blob a session
// names stays however old, a fork's prefix and a session made by an
// import among the namers.
func testBlobSweep(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	sw, ok := st.(agentsession.BlobSweeper)
	if !ok {
		t.Skip("the store does not sweep its blobs")
	}
	bw := st.(agentsession.BlobWriter)
	put := func(text string) string {
		t.Helper()
		h, err := bw.PutBlob(ctx, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	named, stray := put("named"), put("nothing names it")
	if _, err := st.Create(ctx, agentsession.Header{ID: "side", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	e, err := st.Append(ctx, "side", user("see sidecar:"+named))
	if err != nil {
		t.Fatal(err)
	}
	// A fork whose prefix names the blob keeps it once its origin goes.
	if _, err := st.Create(ctx, agentsession.Header{ID: "fork", Media: agentsession.MediaSidecar, Base: e, ParentSession: "side"}); err != nil {
		t.Fatal(err)
	}
	if n, err := sw.SweepBlobs(ctx, time.Hour); err != nil || n != 0 {
		t.Errorf("a sweep with every blob young: %d, %v; want 0", n, err)
	}
	if err := st.Delete(ctx, "side"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if n, err := sw.SweepBlobs(ctx, time.Millisecond); err != nil || n != 1 {
		t.Errorf("a sweep past the grace: %d, %v; want 1", n, err)
	}
	if br, ok := st.(agentsession.BlobReader); ok {
		if _, err := br.Blob(ctx, stray); !errors.Is(err, agentsession.ErrNoBlob) {
			t.Errorf("the blob nothing names: %v, want ErrNoBlob", err)
		}
		if _, err := br.Blob(ctx, named); err != nil {
			t.Errorf("the blob the fork's prefix names: %v", err)
		}
	}
	// Stored again, it is young again.
	if again := put("nothing names it"); again != stray {
		t.Fatalf("PutBlob again: %s, want %s", again, stray)
	}
	if n, err := sw.SweepBlobs(ctx, time.Hour); err != nil || n != 0 {
		t.Errorf("a sweep after the blob was stored again: %d, %v; want 0", n, err)
	}
}

// typed returns st as a store that keeps blob types, failing the test
// for a store that holds blobs and keeps none, since RFC 0002's Media
// section has every store that holds sidecar media keep a blob's type,
// and false for one that holds no blobs.
func typed(t *testing.T, st agentsession.Store) (agentsession.TypedBlobWriter, agentsession.TypedBlobReader, bool) {
	t.Helper()
	if _, ok := st.(agentsession.BlobWriter); !ok {
		return nil, nil, false
	}
	tw, okw := st.(agentsession.TypedBlobWriter)
	tr, okr := st.(agentsession.TypedBlobReader)
	if !okw || !okr {
		t.Errorf("a store that holds blobs keeps no types: TypedBlobWriter %v, TypedBlobReader %v", okw, okr)
		return nil, nil, false
	}
	return tw, tr, true
}

// wantType reads the blob named h and checks its bytes and its type.
func wantType(t *testing.T, tr agentsession.TypedBlobReader, what, h string, data []byte, mediaType string) {
	t.Helper()
	got, mt, err := tr.TypedBlob(context.Background(), h)
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	if !bytes.Equal(got, data) || mt != mediaType {
		t.Errorf("%s: %q of type %q; want %q of type %q", what, got, mt, data, mediaType)
	}
}

// testBlobTypes holds a store to RFC 0002's rule for a blob's media
// type: kept beside the blob, outside its hash, and returned as given;
// once held never changed, by another type or by none; given to a blob
// held with none; none for an empty type and for
// application/octet-stream; and a type that is not one refused.
func testBlobTypes(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	tw, tr, ok := typed(t, st)
	if !ok {
		return
	}
	png := []byte("png bytes")
	const pngType = `image/png; name="a b"`
	h, err := tw.PutTypedBlob(ctx, png, pngType)
	if err != nil {
		t.Fatal(err)
	}
	if want := agentsession.HashBytes(png); h != want {
		t.Errorf("PutTypedBlob: %s, want %s, the hash of the bytes alone", h, want)
	}
	wantType(t, tr, "a typed blob", h, png, pngType)
	if b, err := st.(agentsession.BlobReader).Blob(ctx, h); err != nil || !bytes.Equal(b, png) {
		t.Errorf("Blob of a typed blob: %q, %v", b, err)
	}
	if again, err := tw.PutTypedBlob(ctx, png, "image/jpeg"); err != nil || again != h {
		t.Errorf("PutTypedBlob again with another type: %s, %v", again, err)
	}
	wantType(t, tr, "a typed blob stored again with another type", h, png, pngType)
	if _, err := tw.PutBlob(ctx, png); err != nil {
		t.Fatal(err)
	}
	wantType(t, tr, "a typed blob stored again with none", h, png, pngType)

	raw := []byte("bytes of no known type")
	hr, err := tw.PutBlob(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	wantType(t, tr, "a blob stored with none", hr, raw, "")
	if _, err := tw.PutTypedBlob(ctx, raw, agentsession.DefaultMediaType); err != nil {
		t.Fatal(err)
	}
	wantType(t, tr, "a blob stored with application/octet-stream", hr, raw, "")
	if _, err := tw.PutTypedBlob(ctx, raw, "text/plain;charset=utf-8"); err != nil {
		t.Fatal(err)
	}
	wantType(t, tr, "a blob held with none given a type", hr, raw, "text/plain;charset=utf-8")

	for _, bad := range []string{"png", "image/", "image/png\r\nX-Evil: 1", "image/png; =", " image/png", strings.Repeat("a", 600) + "/" + strings.Repeat("b", 600)} {
		_, err := tw.PutTypedBlob(ctx, []byte("refused "+bad), bad)
		wantErr(t, "PutTypedBlob with "+strconvQuote(bad), err, agentsession.ErrBadMediaType)
	}
	_, _, err = tr.TypedBlob(ctx, agentsession.HashBytes([]byte("never stored")))
	wantErr(t, "TypedBlob of one never stored", err, agentsession.ErrNoBlob)
}

// strconvQuote quotes s for a test's message.
func strconvQuote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return "\"" + strings.ReplaceAll(s, "\r\n", `\r\n`) + "\""
}

// testImportBlobTypes holds an import of a sidecar session to RFC 0002:
// each blob is stored with the type its directory gives it beside it,
// one with no type file with none, and a type file that holds no media
// type refuses the import, which leaves nothing.
func testImportBlobTypes(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	_, tr, ok := typed(t, st)
	if !ok {
		return
	}
	im, ok := st.(agentsession.Importer)
	if !ok {
		t.Skip("the store does not import")
	}
	gif, plain := []byte("gif bytes"), []byte("bytes with no type file")
	hg, hp := agentsession.HashBytes(gif), agentsession.HashBytes(plain)
	sess := agentsession.New(agentsession.Header{ID: "typed-imp", Media: agentsession.MediaSidecar})
	for _, b := range []string{hg, hp} {
		if _, err := sess.Append(user("see sidecar:" + b)); err != nil {
			t.Fatal(err)
		}
	}
	file := fileOf(t, sess)
	hex := func(h string) string { return strings.TrimPrefix(h, agentsession.HashPrefix) }

	bad := fstest.MapFS{
		hex(hg):                          {Data: gif},
		agentsession.SidecarTypeName(hg): {Data: []byte("not a type\n")},
		hex(hp):                          {Data: plain},
	}
	_, _, err := agentsession.ImportMedia(ctx, im, bytes.NewReader(file), bad, true)
	wantErr(t, "an import whose type file holds no media type", err, agentsession.ErrBadMediaType)
	if _, err := st.Open(ctx, "typed-imp"); err == nil {
		t.Error("an import refused for its type file left its session")
	}

	dir := fstest.MapFS{
		hex(hg):                          {Data: gif},
		agentsession.SidecarTypeName(hg): {Data: []byte("image/gif\n")},
		hex(hp):                          {Data: plain},
	}
	if _, unresolved, err := agentsession.ImportMedia(ctx, im, bytes.NewReader(file), dir, true); err != nil || len(unresolved) != 0 {
		t.Fatalf("an import with its directory: %v, unresolved %v", err, unresolved)
	}
	wantType(t, tr, "an imported blob with a type file", hg, gif, "image/gif")
	wantType(t, tr, "an imported blob with none", hp, plain, "")
}

// testBlobTypeSweep holds a store that sweeps its blobs to RFC 0002's
// rule that a blob's type goes with it: bytes swept and stored again
// take the type they are then given, not the one they had.
func testBlobTypeSweep(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	sw, ok := st.(agentsession.BlobSweeper)
	if !ok {
		t.Skip("the store does not sweep its blobs")
	}
	tw, tr, ok := typed(t, st)
	if !ok {
		return
	}
	data := []byte("swept and stored again")
	h, err := tw.PutTypedBlob(ctx, data, "text/x-before")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if n, err := sw.SweepBlobs(ctx, time.Millisecond); err != nil || n != 1 {
		t.Fatalf("a sweep past the grace: %d, %v; want 1", n, err)
	}
	if _, _, err := tr.TypedBlob(ctx, h); !errors.Is(err, agentsession.ErrNoBlob) {
		t.Errorf("the swept blob: %v, want ErrNoBlob", err)
	}
	if _, err := tw.PutTypedBlob(ctx, data, "text/x-after"); err != nil {
		t.Fatal(err)
	}
	wantType(t, tr, "bytes swept and stored again", h, data, "text/x-after")
}
