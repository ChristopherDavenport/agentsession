package remote

import (
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// TestUploadsPruned: what an upload holds for its uploader, its type
// among it, is dropped once the upload grace has passed for it, every
// principal's, not only the one a read asks about, so the record of
// uploads cannot grow without bound.
func TestUploadsPruned(t *testing.T) {
	var u uploads
	stale := objKey{cas.SpaceMedia, agentsession.HashBytes([]byte("stale"))}
	fresh := objKey{cas.SpaceMedia, agentsession.HashBytes([]byte("fresh"))}
	u.by = map[string]map[objKey]upload{"alice": {stale: {at: time.Now().Add(-time.Hour), mediaType: "image/png"}}}
	u.put("bob", fresh, "text/plain")
	if !u.has("bob", fresh, time.Minute) {
		t.Fatal("a fresh upload is not held")
	}
	if n := len(u.by["alice"]); n != 0 {
		t.Errorf("alice's upload past the grace is still held: %d entries", n)
	}
}
