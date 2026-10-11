package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/remote"
)

// lines are a session's header and entry lines, as the format writes
// them.
func lines(t *testing.T, s *agentsession.Session) [][]byte {
	t.Helper()
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'})
}

func idOf(t *testing.T, line []byte) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// snapshotEvent writes a snapshot event of the header and entry lines.
func snapshotEvent(t *testing.T, w http.ResponseWriter, id string, ls [][]byte) {
	t.Helper()
	type rec struct {
		Seq   int             `json:"seq"`
		Entry string          `json:"entry"`
		Line  json.RawMessage `json:"line"`
	}
	var recs []rec
	for i, l := range ls[1:] {
		recs = append(recs, rec{Seq: i + 1, Entry: idOf(t, l), Line: l})
	}
	head := idOf(t, ls[len(ls)-1])
	data, err := json.Marshal(map[string]any{"header": json.RawMessage(ls[0]), "incarnation": "i", "head": head, "records": recs})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "event: snapshot\nid: %s\ndata: %s\n\n", id, data)
}

func appendedEvent(t *testing.T, w http.ResponseWriter, id string, seq int, line []byte) {
	t.Helper()
	entry := idOf(t, line)
	data, err := json.Marshal(map[string]any{"seq": seq, "entry": entry, "line": json.RawMessage(line), "outcome": "continued", "head": entry})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "event: appended\nid: %s\ndata: %s\n\n", id, data)
}

func fake(t *testing.T, fn http.HandlerFunc) *remote.Client {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return dial(t, srv.URL)
}

// An entry whose line does not hash to its ID is refused, in a
// snapshot and in an append: the client never yields it.
func TestATamperedEntryIsRefused(t *testing.T) {
	_, s, _ := seeded(t, 3)
	ls := lines(t, s)
	tamper := func(l []byte) []byte { return bytes.Replace(l, []byte("entry "), []byte("ENTRY "), 1) }
	t.Run("snapshot", func(t *testing.T) {
		bad := append([][]byte{ls[0], ls[1]}, tamper(ls[2]), ls[3])
		c := fake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			snapshotEvent(t, w, "a.0", bad)
		})
		for ch, err := range c.Follow(context.Background(), s.ID(), "") {
			if !errors.Is(err, agentsession.ErrBadID) {
				t.Errorf("change %v, error %v: want ErrBadID", ch.Kind, err)
			}
			break
		}
	})
	t.Run("appended", func(t *testing.T) {
		c := fake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			snapshotEvent(t, w, "a.0", ls[:3])
			appendedEvent(t, w, "b.0", 3, tamper(ls[3]))
		})
		var kinds []agentsession.ChangeKind
		var last error
		for ch, err := range c.Follow(context.Background(), s.ID(), "") {
			if err != nil {
				last = err
				break
			}
			kinds = append(kinds, ch.Kind)
		}
		if len(kinds) != 1 || kinds[0] != agentsession.Snapshot || !errors.Is(last, agentsession.ErrBadID) {
			t.Errorf("changes %v then %v, want the snapshot and ErrBadID", kinds, last)
		}
	})
}

// A stream cut before its terminal frame is an error, never a short
// result: a listing without its end frame, and a follow that stops
// without an error event.
func TestACutStreamIsAnError(t *testing.T) {
	_, s, _ := seeded(t, 2)
	ls := lines(t, s)
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/follow"):
			w.Header().Set("Content-Type", "text/event-stream")
			snapshotEvent(t, w, "a.0", ls)
		case r.URL.Path == "/v1/sessions":
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(w, `{"header":%s,"incarnation":"i"}`+"\n", ls[0])
		case r.URL.Path == "/v1/refs":
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintln(w, `{"name":"a","target":{"session":"s","entry":""}}`)
		}
	})
	ctx := context.Background()
	n := 0
	var last error
	for _, err := range c.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			last = err
			break
		}
		n++
	}
	if n != 1 || !errors.Is(last, remote.ErrCut) {
		t.Errorf("List: %d summaries then %v, want 1 and ErrCut", n, last)
	}
	n, last = 0, nil
	for _, err := range c.ListRefs(ctx, "") {
		if err != nil {
			last = err
			break
		}
		n++
	}
	if n != 1 || !errors.Is(last, remote.ErrCut) {
		t.Errorf("ListRefs: %d refs then %v, want 1 and ErrCut", n, last)
	}
	var kinds []agentsession.ChangeKind
	last = nil
	for ch, err := range c.Follow(ctx, s.ID(), "") {
		if err != nil {
			last = err
			break
		}
		kinds = append(kinds, ch.Kind)
	}
	if len(kinds) != 1 || !errors.Is(last, remote.ErrCut) {
		t.Errorf("Follow: %v then %v, want the snapshot and ErrCut", kinds, last)
	}
}

// A follow the server cuts for falling behind resumes from the last
// event the client read, which it sends as Last-Event-ID, and the
// caller sees no break.
func TestAnOverflowResumesFromTheLastEvent(t *testing.T) {
	_, s, _ := seeded(t, 3)
	ls := lines(t, s)
	var mu sync.Mutex
	var resumedFrom []string
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		resumedFrom = append(resumedFrom, r.Header.Get("Last-Event-ID"))
		n := len(resumedFrom)
		mu.Unlock()
		switch n {
		case 1:
			snapshotEvent(t, w, "a.0", ls[:3])
			fmt.Fprint(w, "event: error\ndata: {\"status\":503,\"code\":\"overflow\",\"title\":\"behind\"}\n\n")
		case 2:
			appendedEvent(t, w, "b.0", 3, ls[3])
			fmt.Fprint(w, "event: error\ndata: {\"status\":404,\"code\":\"no_session\",\"title\":\"gone\"}\n\n")
		}
	})
	var got []string
	var last error
	for ch, err := range c.Follow(context.Background(), s.ID(), "") {
		if err != nil {
			last = err
			break
		}
		got = append(got, ch.Kind.String()+":"+string(ch.Cursor))
	}
	if strings.Join(got, ",") != "snapshot:a.0,appended:b.0" || !errors.Is(last, agentsession.ErrNoSession) {
		t.Errorf("changes %v then %v", got, last)
	}
	if strings.Join(resumedFrom, ",") != ",a.0" {
		t.Errorf("requests resumed from %q, want none and then a.0", resumedFrom)
	}
}
