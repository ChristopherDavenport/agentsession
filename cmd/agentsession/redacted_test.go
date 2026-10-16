package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// redaction says how redactFixture redacts: unmarked writes no entry's
// redacted, as a 0.12 redactor did; a block [from, to] replaces those
// positions with one placeholder carrying [[from, to]].
type redaction struct {
	unmarked bool
	block    [2]int
}

// redactFixture writes to path a redaction of the named fixture, made
// as a redactor makes one with the exported helpers: every body string
// through one Rewriter in file order, every reference re-pointed with
// RemapRefs to the entries already written, each entry marked with the
// position it was made from by SetRedacted and committed, its ID
// cleared, to a session whose header carries redacted and redacted_from
// under the ID RedactedSessionID derives. The request hashes are kept,
// so they are the original's. It returns the session as built, which a
// test may grow, and writes its file.
func redactFixture(t *testing.T, name, path, profile string, extra map[string]json.RawMessage, f func(string) string, how redaction) *agentsession.Session {
	t.Helper()
	orig := loadFixture(t, name)
	oh := orig.Header()
	if oh.Base != "" {
		t.Fatalf("%s has a base; redactFixture does not re-point one", name)
	}
	h := oh
	h.ID = agentsession.RedactedSessionID(oh.ID, profile)
	h.Redacted = true
	h.RedactedFrom = &agentsession.RedactedFrom{Session: oh.ID, Profile: profile, Extra: extra}
	red := agentsession.New(h)
	ids := map[string]string{}
	remap := agentsession.Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != oh.ID {
			return "", false
		}
		id, ok := ids[entry]
		return id, ok
	}}
	commit := func(e agentsession.Entry, spans []agentsession.Span) string {
		t.Helper()
		e.Base().ID = ""
		if !how.unmarked {
			if err := agentsession.SetRedacted(e, spans); err != nil {
				t.Fatal(err)
			}
		}
		res, err := red.Commit(e)
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		return res.ID
	}
	rw := agentsession.NewRewriter(f)
	placeholder := ""
	for i, e := range agentsession.FileOrder(orig) {
		pos := i + 1
		if from, to := how.block[0], how.block[1]; from > 0 && pos >= from && pos <= to {
			if pos == from {
				ph := agentsession.NewItemEntry(openresponses.SystemText(fmt.Sprintf("[entries %d to %d redacted]", from, to)))
				ph.Parent = ids[e.Base().Parent]
				placeholder = commit(ph, []agentsession.Span{{From: from, To: to}})
			}
			ids[e.Base().ID] = placeholder
			continue
		}
		body, err := rw.Rewrite(e)
		if err != nil {
			t.Fatalf("rewrite %s: %v", e.Base().ID, err)
		}
		r, err := agentsession.RemapRefs(body, remap)
		if err != nil {
			t.Fatalf("remap %s: %v", e.Base().ID, err)
		}
		ids[e.Base().ID] = commit(r.Entry, []agentsession.Span{{From: pos, To: pos}})
	}
	writeSession(t, path, red)
	return red
}

// writeSession writes s as its file at path.
func writeSession(t *testing.T, path string, s *agentsession.Session) {
	t.Helper()
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
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

// responsesOf is the response entries of s in file order.
func responsesOf(s *agentsession.Session) []*agentsession.ResponseEntry {
	var out []*agentsession.ResponseEntry
	for _, e := range s.Entries() {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			out = append(out, r)
		}
	}
	return out
}

// verifyOut runs verify on args and returns its stdout and exit code.
func verifyOut(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"verify"}, args...), &stdout, &stderr)
	return stdout.String(), code
}

// TestVerifyRedacted: verify of a redacted file says its ids verify it
// and not its original, names the original from redacted_from, counts
// the entries a redaction made and the native ones, and counts a
// request hash a redacted entry kept that no longer matches apart, as
// expected, failing nothing over it (RFC 0001, Redaction). A native
// entry's request hash is held to the rule as in any session, so a
// resumed redacted session whose native response does not match fails.
// A line whose id does not verify still fails, and a cas store verifies
// each session as its file does. show prints the link, and export
// carries it into the document.
func TestVerifyRedacted(t *testing.T) {
	tmp := t.TempDir()
	orig := loadFixture(t, "basic").Header().ID
	extra := map[string]json.RawMessage{"ex.tool": json.RawMessage(`"scrub/1"`)}

	path := filepath.Join(tmp, "redacted.jsonl")
	red := redactFixture(t, "basic", path, "test/v1", extra, redactFour, redaction{})
	resp := responsesOf(red)
	if len(resp) != 2 {
		t.Fatalf("%d responses in the redaction of basic, want 2", len(resp))
	}
	head := []string{
		"each id checked against its hash: the ids verify this redacted file, not its original",
		"redacted_from session " + orig + ", profile test/v1, also ex.tool",
	}
	want := strings.Join([]string{
		"9 entries read, " + head[0],
		head[1],
		"9 redacted entries, 0 native",
		shortID(resp[0].ID) + "  ok",
		shortID(resp[1].ID) + "  differs from recorded " + resp[1].RequestHash + ", as a redacted entry's request_hash may",
		"1 verified, 0 without hash, 1 redacted differ as expected, 0 failed",
	}, "\n") + "\n"
	if got, code := verifyOut(t, path); got != want || code != 0 {
		t.Errorf("verify of a redacted file: exit %d\n%s\nwant exit 0\n%s", code, got, want)
	}

	// Resumed: a writer appends natively, and its response's request
	// hash, which is its own and not the original's, does not match.
	resumed := filepath.Join(tmp, "resumed.jsonl")
	if _, err := red.Append(agentsession.NewItemEntry(openresponses.UserText("and 3+3?"))); err != nil {
		t.Fatal(err)
	}
	native := &agentsession.ResponseEntry{ResponseID: "resp_3", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted,
		RequestHash: agentsession.HashPrefix + strings.Repeat("0", 64)}
	if _, err := red.Append(native); err != nil {
		t.Fatal(err)
	}
	writeSession(t, resumed, red)
	want = strings.Join([]string{
		"11 entries read, " + head[0],
		head[1],
		"9 redacted entries, 2 native",
		shortID(resp[0].ID) + "  ok",
		shortID(resp[1].ID) + "  differs from recorded " + resp[1].RequestHash + ", as a redacted entry's request_hash may",
		shortID(native.ID) + "  MISMATCH recorded " + native.RequestHash,
		"1 verified, 0 without hash, 1 redacted differ as expected, 1 failed",
	}, "\n") + "\n"
	if got, code := verifyOut(t, resumed); got != want || code != 1 {
		t.Errorf("verify of a resumed redacted file: exit %d\n%s\nwant exit 1\n%s", code, got, want)
	}

	// A block of four entries, the first response among them, left out
	// for one placeholder that names the run.
	block := filepath.Join(tmp, "block.jsonl")
	redactFixture(t, "basic", block, "test/block", nil, redactFour, redaction{block: [2]int{3, 6}})
	if data, _ := os.ReadFile(block); !bytes.Contains(data, []byte(`"redacted":[[3,6]]`)) {
		t.Errorf("the placeholder does not name the run:\n%s", data)
	}

	// A redacted file 0.12 wrote marks no entry and has no
	// redacted_from, and one may hold a redacted_from of its own; every
	// entry of either is the redaction's.
	unmarked := filepath.Join(tmp, "unmarked.jsonl")
	redactFixture(t, "basic", unmarked, "test/v1", nil, redactFour, redaction{unmarked: true})
	v12 := filepath.Join(tmp, "v12.jsonl")
	withHeader(t, unmarked, v12, func(h map[string]json.RawMessage) {
		h["format"] = json.RawMessage(`"agentsession/0.12"`)
		delete(h, "redacted_from")
	})
	v12From := filepath.Join(tmp, "v12-from.jsonl")
	withHeader(t, unmarked, v12From, func(h map[string]json.RawMessage) {
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

	// cas stores hold the redacted session and the resumed one, by
	// import.
	root := filepath.Join(tmp, "cas")
	importFile(t, root, path, "")
	resumedRoot := filepath.Join(tmp, "cas-resumed")
	importFile(t, resumedRoot, resumed, "")

	for _, tt := range []struct {
		name   string
		args   []string
		code   int
		stdout []string
		absent []string
		stderr []string
	}{
		{
			name: "a block placeholder", args: []string{"verify", block},
			stdout: []string{head[0], "6 redacted entries, 0 native", "0 verified, 0 without hash, 1 redacted differ as expected, 0 failed"},
			absent: []string{"MISMATCH"},
		},
		{
			name: "unmarked at 0.13", args: []string{"verify", unmarked}, code: 1,
			stdout: []string{"0 redacted entries, 9 native", "MISMATCH recorded", "1 verified, 0 without hash, 0 redacted differ as expected, 1 failed"},
		},
		{
			name: "0.12 without redacted_from", args: []string{"verify", v12},
			stdout: []string{
				head[0],
				"redacted_from absent: this file names no original",
				"9 redacted entries, 0 native: a redacted file before 0.13 marks no entry and took no appends, so every entry is the redaction's",
				"1 verified, 0 without hash, 1 redacted differ as expected, 0 failed",
			},
			absent: []string{"MISMATCH"},
		},
		{
			name: "0.12 with a redacted_from of its own", args: []string{"verify", v12From},
			stdout: []string{`redacted_from "elsewhere", kept as written and not read as a link: this file names no original`, "0 failed"},
		},
		{name: "a line that does not verify", args: []string{"verify", broken}, code: 1, stderr: []string{"does not match its hash"}},
		{
			name: "the store", args: []string{"verify", root},
			stdout: []string{"1 sessions, 9 entries", " 0 problems", "1 sessions' hashes and records checked, 0 failed"},
			absent: []string{"MISMATCH", "differs"},
		},
		{
			name: "the session in the store", args: []string{"verify", root, red.ID()},
			stdout: []string{head[0], head[1], "9 redacted entries, 0 native", "1 verified, 0 without hash, 1 redacted differ as expected, 0 failed"},
		},
		{
			name: "the store holding the resumed session", args: []string{"verify", resumedRoot}, code: 1,
			stdout: []string{red.ID() + ": " + shortID(native.ID) + "  MISMATCH recorded", "1 sessions' hashes and records checked, 1 failed"},
			absent: []string{shortID(resp[1].ID)},
		},
		{
			name: "show", args: []string{"show", path},
			stdout: []string{"redacted from  session " + orig + ", profile test/v1, also ex.tool"},
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

	// The exported document links back to the original as the file does,
	// and its steps carry the entries' redacted.
	doc, err := os.ReadFile(filepath.Join(tmp, "out", red.ID()+".json"))
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
		Steps []struct {
			Extra struct {
				AgentSession struct {
					Redacted json.RawMessage `json:"redacted"`
				} `json:"agentsession"`
			} `json:"extra"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	if as := d.Extra.AgentSession; !as.Redacted || string(as.RedactedFrom["session"]) != `"`+orig+`"` || string(as.RedactedFrom["ex.tool"]) != `"scrub/1"` {
		t.Errorf("exported extra.agentsession: redacted %v, redacted_from %s", as.Redacted, as.RedactedFrom)
	}
	var first bytes.Buffer
	if len(d.Steps) > 0 {
		_ = json.Compact(&first, d.Steps[0].Extra.AgentSession.Redacted)
	}
	if first.String() != "[2]" {
		t.Errorf("the first step, from the user item at position 2, does not carry [2]:\n%s", doc)
	}
}
