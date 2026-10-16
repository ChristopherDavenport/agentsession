package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

// redactFixture writes to path a redaction of the named fixture, made
// as a redactor makes one with the exported helpers: every body string
// through one Rewriter in file order, every reference re-pointed with
// RemapRefs to the entries already written, each entry committed, its
// ID cleared, to a session whose header carries redacted and
// redacted_from, under the ID RedactedSessionID derives. The request
// hashes are kept, so they are the original's. It returns the file as
// read back.
func redactFixture(t *testing.T, name, path, profile string, extra map[string]json.RawMessage, f func(string) string) *agentsession.Session {
	t.Helper()
	orig := loadFixture(t, name)
	oh := orig.Header()
	if oh.Base != "" {
		t.Fatalf("%s has a base; redactFixture does not re-point one", name)
	}
	order := agentsession.FileOrder(orig)
	h := oh
	h.ID = agentsession.RedactedSessionID(oh.ID, len(order), profile)
	h.Redacted = true
	h.RedactedFrom = &agentsession.RedactedFrom{Session: oh.ID, Entries: len(order), Profile: profile, Aligned: true, Extra: extra}
	red := agentsession.New(h)
	ids := map[string]string{}
	remap := agentsession.Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != oh.ID {
			return "", false
		}
		id, ok := ids[entry]
		return id, ok
	}}
	rw := agentsession.NewRewriter(f)
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
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, red); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return readPath(t, path)
}

// redactFour replaces the function output "4" of the basic fixture, the
// one string the second request holds and the first does not, so the
// redacted file keeps one request hash that matches and one that does
// not.
func redactFour(s string) string {
	if s == "4" {
		return "[n]"
	}
	return s
}

// withHeader writes to dst the file at src with its header line's
// members changed by edit.
func withHeader(t *testing.T, src, dst string, edit func(map[string]json.RawMessage)) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	head, rest, _ := strings.Cut(string(data), "\n")
	var h map[string]json.RawMessage
	if err := json.Unmarshal([]byte(head), &h); err != nil {
		t.Fatal(err)
	}
	edit(h)
	line, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(string(line)+"\n"+rest), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyRedacted: verify of a redacted file says its ids verify it
// and not its original, names the original from redacted_from, and
// counts a request hash that no longer matches apart, as expected of a
// redacted session, failing nothing over it (RFC 0001, Redaction). A
// line whose id does not verify still fails, and a cas store holding
// the session verifies it as clean. show prints the link, and export
// carries it into the document.
func TestVerifyRedacted(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "redacted.jsonl")
	extra := map[string]json.RawMessage{"ex.tool": json.RawMessage(`"scrub/1"`)}
	s := redactFixture(t, "basic", path, "test/v1", extra, redactFour)
	var responses []*agentsession.ResponseEntry
	for _, e := range s.Entries() {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			responses = append(responses, r)
		}
	}
	if len(responses) != 2 {
		t.Fatalf("%d responses in the redaction of basic, want 2", len(responses))
	}
	orig := loadFixture(t, "basic").Header().ID
	want := strings.Join([]string{
		"9 entries read, each id checked against its hash: the ids verify this redacted file, not its original",
		"redacted_from session " + orig + ", 9 entries, profile test/v1, aligned, also ex.tool",
		shortID(responses[0].ID) + "  ok",
		shortID(responses[1].ID) + "  differs from recorded " + responses[1].RequestHash + ", as a redacted session's request_hash may",
		"1 verified, 0 without hash, 1 differ as expected of a redacted session, 0 failed",
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verify", path}, &stdout, &stderr); code != 0 {
		t.Errorf("exit %d, want 0\nstderr:\n%s", code, stderr.String())
	}
	if got := stdout.String(); got != want {
		t.Errorf("verify of a redacted file:\n%s\nwant:\n%s", got, want)
	}

	// A redacted file 0.12 wrote has no redacted_from, and one a later
	// minor could not read the member of keeps it as written; neither
	// names an original.
	v12 := filepath.Join(tmp, "v12.jsonl")
	withHeader(t, path, v12, func(h map[string]json.RawMessage) {
		h["format"] = json.RawMessage(`"agentsession/0.12"`)
		delete(h, "redacted_from")
	})
	v12From := filepath.Join(tmp, "v12-from.jsonl")
	withHeader(t, path, v12From, func(h map[string]json.RawMessage) {
		h["format"] = json.RawMessage(`"agentsession/0.12"`)
		h["redacted_from"] = json.RawMessage(`"elsewhere"`)
	})
	// A body changed after the redaction was written: the line no
	// longer hashes to its id, which is corruption in a redacted file
	// as in any other.
	broken := filepath.Join(tmp, "broken.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"[n]"`)) {
		t.Fatalf("the redaction wrote no replacement:\n%s", data)
	}
	if err := os.WriteFile(broken, bytes.Replace(data, []byte(`"[n]"`), []byte(`"[m]"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	// A cas store holds the redacted session whole, by import.
	root := filepath.Join(tmp, "cas")
	importFile(t, root, path, "")

	for _, tt := range []struct {
		name   string
		args   []string
		code   int
		stdout []string
		absent []string
		stderr []string
	}{
		{
			name: "0.12 without redacted_from", args: []string{"verify", v12},
			stdout: []string{
				"the ids verify this redacted file, not its original",
				"redacted_from absent: this file names no original",
				"1 differ as expected of a redacted session, 0 failed",
			},
			absent: []string{"MISMATCH"},
		},
		{
			name: "0.12 with a redacted_from of its own", args: []string{"verify", v12From},
			stdout: []string{`redacted_from "elsewhere", kept as written and not read as a link: this file names no original`},
		},
		{name: "a line that does not verify", args: []string{"verify", broken}, code: 1, stderr: []string{"does not match its hash"}},
		{
			name: "the store", args: []string{"verify", root},
			stdout: []string{"1 sessions, 9 entries", " 0 problems", "1 sessions' hashes and records checked, 0 failed"},
			absent: []string{"MISMATCH"},
		},
		{
			name: "the session in the store", args: []string{"verify", root, s.ID()},
			stdout: []string{
				"the ids verify this redacted file, not its original",
				"redacted_from session " + orig + ", 9 entries, profile test/v1, aligned, also ex.tool",
				"1 verified, 0 without hash, 1 differ as expected of a redacted session, 0 failed",
			},
		},
		{
			name: "show", args: []string{"show", path},
			stdout: []string{"redacted from  session " + orig + ", 9 entries, profile test/v1, aligned, also ex.tool"},
		},
		{
			name: "show 0.12", args: []string{"show", v12},
			stdout: []string{"redacted from  absent: this file names no original"},
		},
		{name: "export", args: []string{"export", path, "-out", filepath.Join(tmp, "out")}},
		{
			name: "show a session not redacted", args: []string{"show", filepath.Join(fixtures, "basic.jsonl")},
			absent: []string{"redacted"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, stdout.String(), stderr.String())
			}
			for _, w := range tt.stdout {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, stdout.String())
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(stdout.String(), a) {
					t.Errorf("stdout has %q:\n%s", a, stdout.String())
				}
			}
			for _, w := range tt.stderr {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr lacks %q:\n%s", w, stderr.String())
				}
			}
		})
	}

	// The exported document links back to the original as the file does.
	doc, err := os.ReadFile(filepath.Join(tmp, "out", s.ID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Extra struct {
			AgentSession struct {
				Redacted     bool                       `json:"redacted"`
				RedactedFrom map[string]json.RawMessage `json:"redacted_from"`
			} `json:"agentsession"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	if as := d.Extra.AgentSession; !as.Redacted || string(as.RedactedFrom["session"]) != `"`+orig+`"` || string(as.RedactedFrom["ex.tool"]) != `"scrub/1"` {
		t.Errorf("exported extra.agentsession: redacted %v, redacted_from %s", as.Redacted, as.RedactedFrom)
	}
}
