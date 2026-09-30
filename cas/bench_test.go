package cas

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// benchEntries is calls rounds of a user message, a function call, its
// response and an output of outputSize bytes, after a config entry.
func benchEntries(calls, outputSize int) []agentsession.Entry {
	output := strings.Repeat("tool output line\n", outputSize/len("tool output line\n")+1)[:outputSize]
	es := []agentsession.Entry{&agentsession.ConfigEntry{Model: "m"}}
	for i := 0; i < calls; i++ {
		callID, respID := fmt.Sprintf("call_%d", i), fmt.Sprintf("resp_%d", i)
		es = append(es,
			&agentsession.ItemEntry{Item: &openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputText{Text: "run it"}}}},
			&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "run", Arguments: `{"cmd":"make"}`}, ResponseID: respID},
			&agentsession.ResponseEntry{ResponseID: respID, Model: "m", Status: openresponses.ResponseStatusCompleted, RequestHash: "sha256:0"},
			&agentsession.ItemEntry{Item: &openresponses.FunctionCallOutput{CallID: callID, Output: openresponses.FunctionCallOutputData{Text: output}}},
		)
	}
	return es
}

// benchStore fills a store with sessions made by entries and returns
// its root with the ID of the last session.
func benchStore(b *testing.B, sessions int, entries func() []agentsession.Entry) (string, string) {
	b.Helper()
	ctx := context.Background()
	root := b.TempDir()
	st, err := Open(root, WithSync(SyncNever))
	if err != nil {
		b.Fatal(err)
	}
	var id string
	for range sessions {
		sess, err := st.Create(ctx, agentsession.Header{CWD: "/p"})
		if err != nil {
			b.Fatal(err)
		}
		id = sess.Header().ID
		// Each session appends entries of its own: an append fills in
		// the entry's envelope.
		for _, e := range entries() {
			if _, err := st.Append(ctx, id, e); err != nil {
				b.Fatal(err)
			}
		}
		if err := st.Release(id); err != nil {
			b.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		b.Fatal(err)
	}
	return root, id
}

func BenchmarkAppend(b *testing.B) {
	ctx := context.Background()
	var entries []agentsession.Entry
	for _, tc := range []struct {
		name   string
		policy SyncPolicy
	}{
		{"never", SyncNever},
		{"on-response", SyncOnResponse},
		{"every-append", SyncEveryAppend},
	} {
		b.Run(tc.name, func(b *testing.B) {
			st, err := Open(b.TempDir(), WithSync(tc.policy))
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			var id string
			entries = benchEntries(100, 2000)
			n := 0
			for b.Loop() {
				if n%len(entries) == 0 {
					entries = benchEntries(100, 2000)
					sess, err := st.Create(ctx, agentsession.Header{CWD: "/p"})
					if err != nil {
						b.Fatal(err)
					}
					id = sess.Header().ID
				}
				if _, err := st.Append(ctx, id, entries[n%len(entries)]); err != nil {
					b.Fatal(err)
				}
				n++
			}
		})
	}
}

// BenchmarkOpen opens one session of a store holding many: the cost
// should follow the session, not the store.
func BenchmarkOpen(b *testing.B) {
	ctx := context.Background()
	entries := func() []agentsession.Entry { return benchEntries(25, 500) }
	for _, sessions := range []int{10, 100, 1000} {
		root, id := benchStore(b, sessions, entries)
		b.Run(fmt.Sprintf("store/sessions=%d", sessions), func(b *testing.B) {
			for b.Loop() {
				st, err := Open(root, WithReadOnly())
				if err != nil {
					b.Fatal(err)
				}
				st.Close()
			}
		})
		b.Run(fmt.Sprintf("session/sessions=%d", sessions), func(b *testing.B) {
			st, err := Open(root)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			for b.Loop() {
				if _, err := st.Open(ctx, id); err != nil {
					b.Fatal(err)
				}
				if err := st.Release(id); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("list/sessions=%d", sessions), func(b *testing.B) {
			st, err := Open(root, WithReadOnly())
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			for b.Loop() {
				for _, err := range st.List(ctx, agentsession.ListFilter{}) {
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkReuse writes an object the store already holds loose, as a
// repeated content does: the write reads the copy and compares it
// rather than write it again.
func BenchmarkReuse(b *testing.B) {
	for _, size := range []int{4 << 10, 64 << 10, 1 << 20, 16 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			o := newObjects(b.TempDir())
			data := []byte(strings.Repeat("x", size))
			hash := hashBytes(data)
			if err := o.write(spaceContents, hash, data, true); err != nil {
				b.Fatal(err)
			}
			pend := newPendSet()
			b.SetBytes(int64(size))
			for b.Loop() {
				if err := o.writeTo(spaceContents, hash, data, false, pend); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
