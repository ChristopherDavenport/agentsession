package remote_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/openresponses"
)

// A durable follow of a 0.12 redacted session that a redactor resumes:
// the first append raises the header and carries redacted, which 0.13
// defines. The replica is built from the server's session, whose header
// Read raised in memory, so it reads the entry as redacted and holds
// the store's format. (The non-durable follow is the conformance
// suite's RaiseFollowed case.)
func TestADurableFollowReadsARaisedEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	url := server(t, st, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	if _, err := c.Create(ctx, agentsession.Header{ID: "s", Format: "agentsession/0.12", Redacted: true}); err != nil {
		t.Fatal(err)
	}
	reader := dial(t, url)
	seen := make(chan agentsession.Change, 8)
	go func() {
		for ch, err := range reader.FollowCommitted(ctx, "s", "") {
			if err != nil {
				return
			}
			seen <- ch
		}
	}()
	if ch := <-seen; ch.Kind != agentsession.Snapshot {
		t.Fatalf("the first change: %v", ch.Kind)
	}
	e := agentsession.NewItemEntry(openresponses.UserText("[redacted]"))
	if err := agentsession.SetRedacted(e, []agentsession.Span{{From: 5, To: 5}}); err != nil {
		t.Fatal(err)
	}
	id, err := c.Append(ctx, "s", e)
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ch := <-seen:
			got, ok := ch.Session.Entry(id)
			if !ok {
				continue
			}
			if spans, ok := agentsession.RedactedOf(got); !ok || len(spans) != 1 || spans[0] != (agentsession.Span{From: 5, To: 5}) {
				t.Errorf("at the %v change the entry reads as redacted %v %v, want [5]", ch.Kind, spans, ok)
			}
			if f := ch.Session.Header().Format; f != agentsession.Format {
				t.Errorf("at the %v change the replica is at %s, want %s", ch.Kind, f, agentsession.Format)
			}
			return
		case <-ctx.Done():
			t.Fatal("the follow never showed the append")
		}
	}
}

// A path read decodes each entry from its line alone, as UnmarshalEntry
// does, since the read carries no header: in a 0.12 session an entry
// holding redacted reads as redacted there, and GateEntry with the
// format of the session's header reads it as that minor does.
func TestAPathReadIsGatedByTheCaller(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(ctx, agentsession.Header{ID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	e := agentsession.NewItemEntry(openresponses.UserText("hi"))
	if err := agentsession.SetRedacted(e, []agentsession.Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), e); err != nil {
		t.Fatal(err)
	}
	st.Close()
	lowerHeaderFile(t, filepath.Join(root, "sessions", "s", "header"), "agentsession/0.12")
	st, err = cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	url := server(t, st, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	p, err := c.Path(ctx, "s", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("%d entries", len(p.Entries))
	}
	pe := p.Entries[0].Entry
	if _, ok := agentsession.RedactedOf(pe); !ok {
		t.Error("the path entry was gated, though the read carries no header")
	}
	info, err := c.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if info.Header.Format != "agentsession/0.12" {
		t.Fatalf("the session is at %s", info.Header.Format)
	}
	agentsession.GateEntry(info.Header.Format, pe)
	if spans, ok := agentsession.RedactedOf(pe); ok {
		t.Errorf("after GateEntry the entry reads as redacted %v", spans)
	}
}
