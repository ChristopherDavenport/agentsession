package agentsession

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestMemoryOriginGoneBeforeTheLock: an import's origin, picked without
// the store's lock, that is deleted before the lock is taken again is
// looked for again, and the fork is held to the next session holding
// its base, not taken unchecked.
func TestMemoryOriginGoneBeforeTheLock(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	e := NewItemEntry(openresponses.UserText("held by two"))
	var a *Session
	for _, id := range []string{"a", "b"} {
		s := New(Header{ID: id})
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := Write(&buf, s); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Import(ctx, &buf, true); err != nil {
			t.Fatal(err)
		}
		if a == nil {
			a = s
		}
	}
	f, err := Fork(a, e.Base().ID, Header{ID: "f"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	file := strings.Replace(buf.String(), `"parent_session":"a",`, `"media":"sidecar",`, 1)
	defer func() { memoryOriginPicked = nil }()
	memoryOriginPicked = func() {
		memoryOriginPicked = nil
		if err := m.Delete(ctx, "a"); err != nil {
			t.Error(err)
		}
	}
	if _, err := m.Import(ctx, strings.NewReader(file), true); !errors.Is(err, ErrMediaDiffers) {
		t.Errorf("an import whose picked origin went, in another media than the next: %v, want ErrMediaDiffers", err)
	}
}
