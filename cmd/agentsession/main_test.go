package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/openresponses"
)

const fixtures = "../../testdata/sessions"

func TestRun(t *testing.T) {
	tmp := t.TempDir()
	// Entry ids are hashes over the bodies, so a fixture is tampered with
	// by replaying it with a change and letting Append rehash: the file
	// then reads, and only the request hashes disagree.
	tampered := filepath.Join(tmp, "tampered.jsonl")
	replayFixture(t, "basic", tampered, func(e agentsession.Entry) {
		if r, ok := e.(*agentsession.ResponseEntry); ok && r.RequestHash != "" {
			flip := "0"
			if r.RequestHash[len("sha256:")] == '0' {
				flip = "1"
			}
			r.RequestHash = "sha256:" + flip + r.RequestHash[len("sha256:0"):]
		}
	})
	// A session whose responses recorded no hash. Verify reports each
	// with ErrNoHash; the CLI counts those apart from the verified and
	// the failed alike, and does not fail the run over them.
	unhashed := filepath.Join(tmp, "unhashed.jsonl")
	replayFixture(t, "basic", unhashed, func(e agentsession.Entry) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			r.RequestHash = ""
		}
	})
	// Entries are named in the output by the first twelve hex characters
	// of their hash; the fixtures carry readable legacy ids, which is how
	// the expectations name them.
	sid := func(fixture, legacy string) string {
		s := loadFixture(t, fixture)
		if strings.Contains(fixture, string(filepath.Separator)) {
			s = readPath(t, fixture)
		}
		for _, e := range s.Entries() {
			if e.Base().LegacyID == legacy {
				return shortID(e.Base().ID)
			}
		}
		t.Fatalf("no entry %s in %s", legacy, fixture)
		return ""
	}
	digest := func(fixture, legacy string) string {
		s := loadFixture(t, fixture)
		for _, e := range s.Entries() {
			if e.Base().LegacyID == legacy {
				return strings.TrimPrefix(e.Base().ID, agentsession.HashPrefix)
			}
		}
		t.Fatalf("no entry %s in %s", legacy, fixture)
		return ""
	}
	root := filepath.Join(tmp, "root", "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"basic", "branch"} {
		src, _ := os.ReadFile(filepath.Join(fixtures, name+".jsonl"))
		if err := os.WriteFile(filepath.Join(root, "2026-09-17T12-00-00.000Z_"+name+".jsonl"), src, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(tmp, "out")
	// A 0.9 file that repeats a call ID, a rule 0.9 gained after its
	// first writer shipped.
	repeated := filepath.Join(tmp, "repeated.jsonl")
	{
		lines := []string{`{"type":"session","format":"agentsession/0.9","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`}
		parent := "null"
		for _, body := range []string{
			`"type":"item","item":{"type":"function_call","id":"f1","call_id":"x","name":"t","arguments":"{}"}`,
			`"type":"item","item":{"type":"function_call","id":"f2","call_id":"x","name":"t","arguments":"{}"}`,
		} {
			l := `{` + body + `,"parent":` + parent + `,"ts":"2026-09-17T16:00:01Z"}`
			id, _, err := agentsession.EntryHashes([]byte(l))
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, `{"id":"`+id+`",`+l[1:])
			parent = `"` + id + `"`
		}
		if err := os.WriteFile(repeated, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		args    []string
		code    int
		stdout  []string // substrings that must appear
		absent  []string // substrings that must not appear on stdout
		stderr  []string
		written []string // files that must exist afterwards
	}{
		{name: "no command", code: 2, stderr: []string{"usage: agentsession"}},
		{name: "unknown command", args: []string{"frob"}, code: 2, stderr: []string{`unknown command "frob"`}},
		{name: "help", args: []string{"help"}, stdout: []string{"usage: agentsession"}},
		{name: "command help", args: []string{"show", "-h"}, stderr: []string{"usage: agentsession show"}},
		{name: "show missing file", args: []string{"show", filepath.Join(tmp, "nope.jsonl")}, code: 1, stderr: []string{"no such file"}},
		{name: "show too many", args: []string{"show", "a", "b"}, code: 2, stderr: []string{"a is not a cas store"}},
		{
			name: "show", args: []string{"show", filepath.Join(fixtures, "branch.jsonl")},
			stdout: []string{"name     Branching demo", "fork×2 [fork]", sid("branch", "r0000002") + "  " + sid("branch", "i0000004") + "  response", "leaf", "context at " + sid("branch", "n0000001"), `system: "An earlier attempt`, `user: "follow-up B"`},
			absent: []string{"truncated"},
		},
		{
			name: "show at leaf", args: []string{"show", "-leaf", "r0000002", filepath.Join(fixtures, "branch.jsonl")},
			stdout: []string{"context at " + sid("branch", "r0000002"), `user: "follow-up A"`},
			absent: []string{`    4  user: "follow-up B"`},
		},
		{name: "show bad leaf", args: []string{"show", filepath.Join(fixtures, "branch.jsonl"), "-leaf", "zz"}, code: 1, stderr: []string{"no such entry"}},
		{
			name: "show compaction", args: []string{"show", filepath.Join(fixtures, "compaction.jsonl")},
			stdout: []string{"compaction", "first kept", "replace, model gpt-5-nano"},
		},
		{
			name: "show truncated", args: []string{"show", filepath.Join(fixtures, "truncated.jsonl")},
			stdout: []string{"truncated: line 10"},
		},
		{name: "verify", args: []string{"verify", filepath.Join(fixtures, "branch.jsonl")}, stdout: []string{sid("branch", "r0000001") + "  ok", "3 verified, 0 without hash, 0 failed"}},
		{name: "verify mismatch", args: []string{"verify", tampered}, code: 1, stdout: []string{"MISMATCH", "2 failed"}},
		{
			name: "verify without a recorded hash", args: []string{"verify", unhashed},
			stdout: []string{sid(unhashed, "r0000001") + "  no hash recorded", "0 verified, 2 without hash, 0 failed"},
			absent: []string{"ok", "MISMATCH", "ERROR"},
		},
		{
			name: "verify pinned", args: []string{"verify", filepath.Join(fixtures, "pinned.jsonl")},
			stdout: []string{"3 verified, 0 without hash, 0 failed"},
		},
		{
			// The pinned item is in context right after the summary,
			// which is where the request that was sent had it.
			name: "show pinned", args: []string{"show", filepath.Join(fixtures, "pinned.jsonl")},
			stdout: []string{
				"first kept " + sid("pinned", "i0000004"), "2 pinned",
				`1  system: "Summary: the user said first`,
				`2  developer: "House rule: never use Box::leak."`,
				`3  developer: "House rule: always run the linter."`,
			},
		},
		{name: "verify a 0.9 file an early writer may have written", args: []string{"verify", repeated}, code: 1, stdout: []string{"call ID repeated", "note: draft 0.9 gained these rules"}},
		{name: "verify truncated", args: []string{"verify", filepath.Join(fixtures, "truncated.jsonl")}, code: 1, stdout: []string{"truncated: line 10"}},
		{name: "verify runs", args: []string{"verify", filepath.Join(fixtures, "runs.jsonl")}, stdout: []string{"2 verified, 0 without hash, 0 failed"}, absent: []string{"records to"}},
		{name: "verify bad records", args: []string{"verify", filepath.Join(fixtures, "bad-records.jsonl")}, code: 1, stdout: []string{"records to " + sid("bad-records", "i0000003") + "  ERROR", "call call_1 has an output and no dispatch"}},
		{
			name: "show runs", args: []string{"show", filepath.Join(fixtures, "runs.jsonl")},
			stdout: []string{"records  run, dispatch, decision", "start run-1 input ref", "hold call_1 by policy", "call_2 to tool", "reject call_3 by policy", "end run-1 input_required pending call_1", "end run-2 done", "eval on " + sid("runs", "r0000002") + " score 0.9 pass", "container sha256:9f2c1e4b7a0d…"},
		},
		{
			name: "show instructions parts", args: []string{"show", filepath.Join(fixtures, "instructions.jsonl")},
			stdout: []string{
				"instructions [product(40B) agentsmd(61B) agentmemory(52B)]",
				"instructions [product= agentsmd= agentmemory(73B)]",
				"omitted service/AGENTS.md (budget), memory/2026-08 (stale)",
				"product 40B from product, agentsmd 61B from agentsmd",
				"service/AGENTS.md 4096B (budget)",
			},
		},
		{
			name: "verify instructions parts", args: []string{"verify", filepath.Join(fixtures, "instructions.jsonl")},
			stdout: []string{"2 verified, 0 without hash, 0 failed"},
		},
		{
			name: "show queued", args: []string{"show", "../../testdata/sessions/queued.jsonl"},
			stdout: []string{
				"records  run, dispatch, decision, queued",
				`steer user: "and skip the smoke tests" from human slack:1758412800.0002 via gateway ref "inbox-1"`,
				`followup user: "then tag the release"`,
				`user: "and skip the smoke tests" from human`,
			},
		},
		{
			name: "show custom entries", args: []string{"show", filepath.Join(fixtures, "interleaved.jsonl")},
			stdout: []string{"custom    agentpolicy (39 bytes)"},
			absent: []string{"(unknown entry type)"},
		},
		{
			name: "show custom entries in full", args: []string{"show", "-v", filepath.Join(fixtures, "interleaved.jsonl")},
			stdout: []string{`agentpolicy {"rule":"bash(ls:*)","verdict":"allow"}`},
			absent: []string{"(unknown entry type)"},
		},
		{
			name: "show an extension entry", args: []string{"show", filepath.Join(fixtures, "extensions.jsonl")},
			stdout: []string{"agentturn:note  extension ("},
			absent: []string{"(unknown entry type)"},
		},
		{name: "export bad prefer", args: []string{"export", filepath.Join(fixtures, "branch.jsonl"), "-out", out, "-prefer", "best"}, code: 2, stderr: []string{`-prefer "best"`}},
		{
			name: "export prefer label", args: []string{"export", filepath.Join(fixtures, "branch.jsonl"), "-out", filepath.Join(tmp, "out2"), "-prefer", "label=fork", "-prefer", "leaf", "-prefer", "score", "-prefer", "latest"},
			written: []string{filepath.Join(tmp, "out2", "01995b2a-0000-7000-8000-000000000003.json")},
		},
		{name: "export without out", args: []string{"export", filepath.Join(fixtures, "branch.jsonl")}, code: 2, stderr: []string{"-out is required"}},
		{
			name: "export", args: []string{"export", filepath.Join(fixtures, "branch.jsonl"), "-out", out, "-secret", "A1", "-redact-home", "-redact-env"},
			stdout:  []string{filepath.Join(out, "01995b2a-0000-7000-8000-000000000003.json"), "_" + digest("branch", "r0000002") + ".json"},
			written: []string{filepath.Join(out, "01995b2a-0000-7000-8000-000000000003.json"), filepath.Join(out, "01995b2a-0000-7000-8000-000000000003_"+digest("branch", "r0000002")+".json")},
		},
		{name: "list", args: []string{"list", filepath.Join(tmp, "root")}, stdout: []string{"CREATED", "01995b2a-0000-7000-8000-000000000003  Branching demo", "01995b2a-0000-7000-8000-000000000001", "/home/u/proj"}},
		{name: "list limit", args: []string{"list", filepath.Join(tmp, "root"), "-limit", "1"}, stdout: []string{"01995b2a-0000-7000-8000-000000000003"}, absent: []string{"01995b2a-0000-7000-8000-000000000001"}},
		{name: "list cwd", args: []string{"list", "-cwd", "/elsewhere", filepath.Join(tmp, "root")}, absent: []string{"01995b2a"}},
		{name: "list harness", args: []string{"list", "-harness", "fixture", filepath.Join(tmp, "root")}, stdout: []string{"01995b2a-0000-7000-8000-000000000003", "01995b2a-0000-7000-8000-000000000001"}},
		{name: "list another harness", args: []string{"list", "-harness", "nope", filepath.Join(tmp, "root")}, absent: []string{"01995b2a"}},
		{name: "list top level", args: []string{"list", "-top-level", filepath.Join(tmp, "root")}, stdout: []string{"01995b2a-0000-7000-8000-000000000003", "01995b2a-0000-7000-8000-000000000001"}},
		{name: "list missing root", args: []string{"list", filepath.Join(tmp, "nope")}, code: 1, stderr: []string{"no such file"}},
		{name: "list file root", args: []string{"list", tampered}, code: 1, stderr: []string{"not a directory"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, stdout.String(), stderr.String())
			}
			for _, want := range tt.stdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(stdout.String(), absent) {
					t.Errorf("stdout has %q:\n%s", absent, stdout.String())
				}
			}
			for _, want := range tt.stderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
				}
			}
			for _, path := range tt.written {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("not written: %v", err)
				}
			}
		})
	}

	// The exported documents carry the redactions.
	doc, err := os.ReadFile(filepath.Join(out, "01995b2a-0000-7000-8000-000000000003.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(doc, []byte(`"A1"`)) || !bytes.Contains(doc, []byte("REDACTED")) {
		t.Error("secret A1 not redacted from the export")
	}
}

func TestDescribeText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", `""`},
		{"  a \n\t b  ", `"a b"`},
		{strings.Repeat("x", 80), `"` + strings.Repeat("x", 71) + `…"`},
	}
	for _, tt := range tests {
		if got := describeText(tt.in); got != tt.want {
			t.Errorf("describeText(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// TestDescribeEveryEntryType: every core entry type renders as
// something a reader can act on. A custom entry is the one a policy
// layer writes and the one show used to print as "(unknown entry
// type)", four identical lines for four different verdicts.
func TestDescribeEveryEntryType(t *testing.T) {
	label := "checkpoint"
	score := 0.5
	tests := []struct {
		entry agentsession.Entry
		want  string
		full  string // what -v adds, when it differs
	}{
		{entry: agentsession.NewItemEntry(openresponses.UserText("hi")), want: `user: "hi"`},
		{entry: &agentsession.ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: "completed"}, want: "resp_1 gpt-5 completed"},
		{entry: &agentsession.ConfigEntry{Model: "gpt-5"}, want: "model gpt-5"},
		{entry: &agentsession.ConfigEntry{InstructionsOmitted: []agentsession.OmittedPart{}}, want: "omitted cleared"},
		{entry: &agentsession.ConfigEntry{InstructionsOmitted: []agentsession.OmittedPart{{ID: "m/1", Reason: "budget"}, {Keep: 474}}}, want: "omitted m/1 (budget), +474"},
		{entry: &agentsession.CompactionEntry{FirstKept: "i1", Summary: openresponses.UserText("so far")}, want: "first kept i1"},
		{entry: &agentsession.BranchSummaryEntry{From: "i1", Summary: openresponses.UserText("before")}, want: "from i1"},
		{entry: agentsession.NewRunStart("run-1", agentsession.SourceInput, "cron:x"), want: "start run-1 input"},
		{entry: agentsession.NewRunEnd("run-1", agentsession.ReasonStopped, "max_turns", nil), want: "end run-1 stopped"},
		{entry: agentsession.NewDispatch("call_1", "i2"), want: "call_1 to tool"},
		{entry: agentsession.NewDispatch("call_1", "i2").WithIdempotencyKey("k-1"), want: `call_1 to tool, key "k-1"`},
		{entry: agentsession.NewDecision("call_1", "i2", agentsession.VerdictAnswer, agentsession.ByPolicy), want: "answer call_1 by policy"},
		{entry: agentsession.NewDecision("call_1", "i2", agentsession.VerdictHold, agentsession.ByPolicy), want: "hold call_1 by policy"},
		{entry: agentsession.NewQueued(openresponses.UserText("steer"), agentsession.ModeSteer).WithTrigger("human", "slack:1", "gateway"), want: `steer user: "steer" from human slack:1 via gateway`},
		{entry: agentsession.NewLabelEntry("i1", label), want: "i1 = checkpoint"},
		{entry: &agentsession.InfoEntry{Name: "Refactor auth"}, want: `name "Refactor auth"`},
		{entry: &agentsession.EnvEntry{CWD: "/p"}, want: "cwd /p"},
		{entry: &agentsession.OutcomeEntry{Kind: agentsession.OutcomeEval, Target: "r1", Score: &score}, want: "eval on r1 score 0.5"},
		{entry: agentsession.NewLinkEntry(agentsession.RelSubsession, "child"), want: "subsession child"},
		{
			entry: &agentsession.CustomEntry{NS: "agentpolicy", Data: []byte(`{"verdict":"deny","rule":"bash(curl:*)"}`)},
			want:  "agentpolicy (40 bytes)",
			full:  `agentpolicy {"verdict":"deny","rule":"bash(curl:*)"}`,
		},
		{
			entry: &agentsession.UnknownEntry{Type: "acme:note", Raw: []byte(`{"type":"acme:note","text":"x"}`)},
			want:  "extension (31 bytes)",
			full:  `extension {"type":"acme:note","text":"x"}`,
		},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.entry.EntryType()+" "+tt.want, func(t *testing.T) {
			seen[tt.entry.EntryType()] = true
			got := describeEntry(tt.entry, false)
			if !strings.Contains(got, tt.want) {
				t.Errorf("describeEntry = %q, want %q", got, tt.want)
			}
			if got == "(unknown entry type)" {
				t.Errorf("%s renders as an unknown type", tt.entry.EntryType())
			}
			want := tt.want
			if tt.full != "" {
				want = tt.full
			}
			if got := describeEntry(tt.entry, true); !strings.Contains(got, want) {
				t.Errorf("describeEntry -v = %q, want %q", got, want)
			}
		})
	}
	// Every core type is in the table, so a type added to the format
	// is a failing test here rather than a blank line in a listing.
	for _, typ := range []string{
		agentsession.TypeItem, agentsession.TypeResponse, agentsession.TypeConfig,
		agentsession.TypeCompaction, agentsession.TypeBranchSummary, agentsession.TypeRun,
		agentsession.TypeDispatch, agentsession.TypeDecision, agentsession.TypeQueued,
		agentsession.TypeLabel, agentsession.TypeInfo, agentsession.TypeEnv,
		agentsession.TypeOutcome, agentsession.TypeLink, agentsession.TypeCustom,
	} {
		if !seen[typ] {
			t.Errorf("no case for the %s entry", typ)
		}
	}
}

// loadFixture reads a session fixture.
func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	if strings.Contains(name, string(filepath.Separator)) {
		return readPath(t, name)
	}
	return readPath(t, filepath.Join(fixtures, name+".jsonl"))
}

// readPath reads the session file at path.
func readPath(t *testing.T, path string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// replayFixture writes a copy of a fixture to path, appending each entry
// afresh after mutate has changed it, so the copy's ids are the hashes
// of what it carries.
func replayFixture(t *testing.T, name, path string, mutate func(agentsession.Entry)) {
	t.Helper()
	src := loadFixture(t, name)
	dst := agentsession.New(src.Header())
	ids := map[string]string{}
	for _, e := range src.Entries() {
		old := e.Base().ID
		mutate(e)
		e.Base().ID = ""
		if p := e.Base().Parent; p != "" {
			e.Base().Parent = ids[p]
		}
		id, err := dst.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		ids[old] = id
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// casStore imports the named fixtures into a new cas store and closes
// it, so nothing is held when the test's commands run.
func casStore(t *testing.T, root string, names ...string) {
	t.Helper()
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		f, err := os.Open(filepath.Join(fixtures, name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.Import(context.Background(), f, true)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Packed, so the commands read through a pack as well as loose.
	if _, err := st.Pack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCAS: every command reads a session a cas store holds, through
// the store and without its lock, and reports on it as it would on the
// file the session projects to. A path inside a session's directory is
// the session, not the one-line header file found there (#105).
func TestCAS(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "cas")
	casStore(t, root, "basic", "branch")
	const id = "01995b2a-0000-7000-8000-000000000003" // branch

	// A writer holds the session open, and its lock, while every
	// command below runs.
	writer, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Open(ctx, id); err != nil {
		t.Fatal(err)
	}
	projected, err := writer.ProjectDir(ctx, filepath.Join(tmp, "projected"), id)
	if err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(tmp, "empty-store")
	casStore(t, empty)
	headerOnly := filepath.Join(tmp, "header-only.jsonl")
	src, err := os.ReadFile(filepath.Join(fixtures, "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerOnly, src[:bytes.IndexByte(src, '\n')+1], 0o600); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(root, "sessions", id)
	out := filepath.Join(tmp, "out")
	// A 0.9 file that repeats a call ID, a rule 0.9 gained after its
	// first writer shipped.
	repeated := filepath.Join(tmp, "repeated.jsonl")
	{
		lines := []string{`{"type":"session","format":"agentsession/0.9","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`}
		parent := "null"
		for _, body := range []string{
			`"type":"item","item":{"type":"function_call","id":"f1","call_id":"x","name":"t","arguments":"{}"}`,
			`"type":"item","item":{"type":"function_call","id":"f2","call_id":"x","name":"t","arguments":"{}"}`,
		} {
			l := `{` + body + `,"parent":` + parent + `,"ts":"2026-09-17T16:00:01Z"}`
			id, _, err := agentsession.EntryHashes([]byte(l))
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, `{"id":"`+id+`",`+l[1:])
			parent = `"` + id + `"`
		}
		if err := os.WriteFile(repeated, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		args    []string
		code    int
		stdout  []string // substrings that must appear
		absent  []string // substrings that must not appear on stdout
		stderr  []string
		written []string // files that must exist afterwards
	}{
		{name: "list", args: []string{"list", root}, stdout: []string{"CREATED", id + "  Branching demo", "01995b2a-0000-7000-8000-000000000001", sessionDir}},
		{name: "list limit", args: []string{"list", root, "-limit", "1"}, stdout: []string{id}, absent: []string{"01995b2a-0000-7000-8000-000000000001"}},
		{name: "show", args: []string{"show", root, id}, stdout: []string{"name     Branching demo", "fork×2 [fork]"}},
		{name: "show at leaf", args: []string{"show", root, id, "-leaf", "r0000002"}, stdout: []string{`user: "follow-up A"`}},
		{
			name: "show the session directory", args: []string{"show", sessionDir},
			stdout: []string{"name     Branching demo"}, stderr: []string{"reading session " + id + " through the store"},
		},
		{name: "show a store", args: []string{"show", root}, code: 2, stderr: []string{"is a cas store; name a session: agentsession show " + root + " <id>"}},
		{name: "show no session", args: []string{"show", root, "nope"}, code: 1, stderr: []string{"no such session"}},
		{name: "show an id after a file", args: []string{"show", projected, id}, code: 2, stderr: []string{"is not a cas store"}},
		{name: "verify", args: []string{"verify", root, id}, stdout: []string{"entries read, each id checked against its hash", "3 verified, 0 without hash, 0 failed"}},
		{
			name: "verify the header file", args: []string{"verify", filepath.Join(sessionDir, "header")},
			stdout: []string{"3 verified, 0 without hash, 0 failed"}, stderr: []string{"reading session " + id + " through the store"},
		},
		{name: "verify the store", args: []string{"verify", root}, stdout: []string{"2 sessions,", "0 problems"}},
		{name: "verify an empty store", args: []string{"verify", empty}, code: 1, stdout: []string{"nothing to verify: " + empty + " holds no sessions"}},
		{name: "verify a header alone", args: []string{"verify", headerOnly}, code: 1, stdout: []string{"nothing to verify: " + headerOnly + " holds a header and no entries"}, absent: []string{"0 failed"}},
		{name: "export a store", args: []string{"export", root, "-out", out}, code: 2, stderr: []string{"name a session"}},
		{
			name: "export", args: []string{"export", root, id, "-out", out},
			stdout: []string{filepath.Join(out, id+".json")}, written: []string{filepath.Join(out, id+".json")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, stdout.String(), stderr.String())
			}
			for _, want := range tt.stdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(stdout.String(), absent) {
					t.Errorf("stdout has %q:\n%s", absent, stdout.String())
				}
			}
			for _, want := range tt.stderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
				}
			}
			for _, path := range tt.written {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("not written: %v", err)
				}
			}
		})
	}

	// Read through the store, a session reports exactly as the file it
	// projects to does.
	for _, cmd := range []string{"show", "verify"} {
		var fromStore, fromFile, stderr bytes.Buffer
		if code := run([]string{cmd, root, id}, &fromStore, &stderr); code != 0 {
			t.Fatalf("%s from the store: exit %d: %s", cmd, code, stderr.String())
		}
		if code := run([]string{cmd, projected}, &fromFile, &stderr); code != 0 {
			t.Fatalf("%s of the projection: exit %d: %s", cmd, code, stderr.String())
		}
		if fromStore.String() != fromFile.String() {
			t.Errorf("%s differs between the store and its projection:\n%s\nvs\n%s", cmd, fromStore.String(), fromFile.String())
		}
	}
}

// TestCASVerifyFindsDamage: verify of a cas root reports what the
// store's own walk finds, and fails on it.
func TestCASVerifyFindsDamage(t *testing.T) {
	root := t.TempDir()
	casStore(t, root, "basic")
	var packs []string
	err := filepath.WalkDir(filepath.Join(root, "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".pack") {
			packs = append(packs, p)
		}
		return err
	})
	if err != nil || len(packs) == 0 {
		t.Fatalf("no pack to damage: %v", err)
	}
	data, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(packs[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verify", root}, &stdout, &stderr); code != 1 {
		t.Errorf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "corrupt") {
		t.Errorf("stdout names no corrupt object:\n%s", stdout.String())
	}
}

// TestCASVerifyLeftover: verify of a cas root prints a loose object
// that fails its name and that nothing the store holds needs, and does
// not fail on it (#171).
func TestCASVerifyLeftover(t *testing.T) {
	root := t.TempDir()
	casStore(t, root, "basic")
	hex := strings.Repeat("ab", 32)
	p := filepath.Join(root, "objects", "contents", hex[:2], hex[2:])
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verify", root}, &stdout, &stderr); code != 0 {
		t.Errorf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"leftover sha256:" + hex, "0 problems, 1 leftover objects nothing needs"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
}

// TestCASVerifyChecksRecords: verify of a cas root runs each session's
// request hash and records checks, as verify of one session does, and
// fails on a session the store's own walk finds sound (#133).
func TestCASVerifyChecksRecords(t *testing.T) {
	tmp := t.TempDir()
	tampered := filepath.Join(tmp, "tampered.jsonl")
	replayFixture(t, "basic", tampered, func(e agentsession.Entry) {
		if r, ok := e.(*agentsession.ResponseEntry); ok && r.RequestHash != "" {
			r.RequestHash = "sha256:" + strings.Repeat("0", 64)
		}
	})
	root := filepath.Join(tmp, "store")
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(tampered)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Import(context.Background(), f, true)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verify", root}, &stdout, &stderr); code != 1 {
		t.Errorf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "MISMATCH") || !strings.Contains(stdout.String(), "1 failed") {
		t.Errorf("stdout names no failing session:\n%s", stdout.String())
	}
	// A sound store still passes, and lists no response.
	clean := filepath.Join(tmp, "clean")
	casStore(t, clean, "basic")
	stdout.Reset()
	if code := run([]string{"verify", clean}, &stdout, &stderr); code != 0 {
		t.Errorf("a sound store: exit %d\n%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), " ok\n") {
		t.Errorf("a store's verify lists each response:\n%s", stdout.String())
	}
}

// TestCASRepair: repair -dry-run reports what a repair of a damaged
// log would keep and writes nothing; repair rewrites the log, keeping
// the damaged one, and the session reads again (#153).
func TestCASRepair(t *testing.T) {
	root := t.TempDir()
	casStore(t, root, "basic")
	const id = "01995b2a-0000-7000-8000-000000000001"
	path := filepath.Join(root, "sessions", id, "log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	last := -1
	for i, l := range lines {
		if strings.Contains(l, `"op":"append"`) {
			last = i
		}
	}
	l := []byte(lines[last])
	l[len(l)/2] ^= 1
	lines[last] = string(l)
	damaged := []byte(strings.Join(lines, ""))
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"show", root, id}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "damaged") {
		t.Errorf("show of a damaged session: exit %d: %s", code, stderr.String())
	}
	for _, dry := range []bool{true, false} {
		args := []string{"repair", root, id}
		if dry {
			args = append(args, "-dry-run")
		}
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout.String(), stderr.String())
		}
		want := []string{fmt.Sprintf("damaged  line %d", last+1), "kept     ", "head     ", "mark     record"}
		if dry {
			want = append(want, "dry run: nothing written")
		} else {
			want = append(want, "the damaged log is kept as "+filepath.Join(root, "sessions", id, "damaged-"))
		}
		for _, w := range want {
			if !strings.Contains(stdout.String(), w) {
				t.Errorf("%v: stdout lacks %q:\n%s", args, w, stdout.String())
			}
		}
		if after, _ := os.ReadFile(path); dry != bytes.Equal(after, damaged) {
			t.Errorf("%v: the log changed: %v", args, !dry)
		}
	}
	stdout.Reset()
	if code := run([]string{"show", root, id}, &stdout, &stderr); code != 0 {
		t.Errorf("show after the repair: exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"verify", root}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "the damaged log is kept as damaged-") {
		t.Errorf("verify after the repair: exit %d:\n%s", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"repair", root, id}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "nothing to repair") {
		t.Errorf("a second repair: exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"repair", root}, &stdout, &stderr); code != 2 {
		t.Errorf("repair without an id: exit %d", code)
	}
}

// legacyCASStore lays the sessions of a new cas store out as a store
// v0.0.15 wrote them after compacting its journal: each log a list of
// entry hashes, an empty store-wide journal, and no layout file.
func legacyCASStore(t *testing.T, root string, names ...string) {
	t.Helper()
	casStore(t, root, names...)
	dirs, err := os.ReadDir(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		path := filepath.Join(root, "sessions", d.Name(), "log")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var legacy []byte
		for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var rec struct{ Op, Entry string }
			if err := json.Unmarshal(l, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Op == "append" {
				legacy = append(legacy, rec.Entry+"\n"...)
			}
		}
		if err := os.WriteFile(path, legacy, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"layout", "journal"} {
		os.Remove(filepath.Join(root, name))
	}
	if err := os.WriteFile(filepath.Join(root, "journal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCASMigrate: no read-only command reads a session of a store
// v0.0.15 wrote, and says to migrate it; migrate does, after which
// every command reads the store (#164).
func TestCASMigrate(t *testing.T) {
	root := t.TempDir()
	legacyCASStore(t, root, "basic", "branch")
	const id = "01995b2a-0000-7000-8000-000000000001"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"show", root, id}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "agentsession migrate") {
		t.Errorf("show of an unmigrated session: exit %d: %s", code, stderr.String())
	}
	// list prints each session's error and then says once what to do
	// about them; the library's own text says it too, since a product
	// that lists a store passes that on (#190).
	stderr.Reset()
	if code := run([]string{"list", root}, &stdout, &stderr); code != 1 || strings.Count(stderr.String(), "agentsession migrate") != 3 || strings.Count(stderr.String(), legacyHint) != 1 {
		t.Errorf("list of an unmigrated store: exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "open the store for writing") {
		t.Errorf("list of an unmigrated store says to open it for writing: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", root}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "agentsession migrate "+root) {
		t.Errorf("verify of an unmigrated store: exit %d:\n%s", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"migrate", root}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "migrated" {
		t.Fatalf("migrate: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"verify", root}, &stdout, &stderr); code != 0 {
		t.Errorf("verify after migrating: exit %d:\n%s", code, stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"show", root, id}, &stdout, &stderr); code != 0 {
		t.Errorf("show after migrating: exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"migrate", root}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "nothing to migrate" {
		t.Errorf("a second migrate: exit %d: %s", code, stdout.String())
	}
	if code := run([]string{"migrate", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Errorf("migrate of a directory that is no store: exit %d", code)
	}
}

// TestNoteDeclared: the note that an early 0.9 writer may have broken
// a rule goes with a file that declared 0.9 when read; a file of an
// earlier minor, which Read raised, is told its minor did not forbid
// it; a 0.10 file earns none (#134).
func TestNoteDeclared(t *testing.T) {
	for declared, want := range map[string]string{
		"agentsession/0.8":  earlierNote,
		"agentsession/0.5":  earlierNote,
		"agentsession/0.9":  earlyNote,
		agentsession.Format: "",
	} {
		if got := noteFor(declared, agentsession.ErrCallIDRepeated); got != want {
			t.Errorf("%s: note %q, want %q", declared, got, want)
		}
	}
	if got := noteFor("agentsession/0.8", agentsession.ErrHashMismatch); got != "" {
		t.Errorf("a hash mismatch in a 0.8 file earns %q", got)
	}
}

// TestVerifyEmptyResume: a resume refused before it takes up its
// call, or cut and closed on restart, fails VerifyRecords as 0.10 has
// it, and verify notes that it took up nothing; a resume that adds a
// message gets the ordinary note (#172).
func TestVerifyEmptyResume(t *testing.T) {
	tmp := t.TempDir()
	write := func(name string, tail func(must appender, end func(reason string), call string)) string {
		path := filepath.Join(tmp, name+".jsonl")
		writeResumed(t, path, tail)
		return path
	}
	// A subscriber refuses the resume.
	refused := write("refused", refusedResume)
	// The process is killed after the resume's start; the restart closes
	// the run error, and the next resume takes the call up.
	cut := write("cut", func(must appender, end func(string), call string) {
		end(agentsession.ReasonError)
		must(agentsession.NewRunStart("run-3", agentsession.SourceResume, ""))
		must(agentsession.NewDecision("a", call, agentsession.VerdictProceed, agentsession.ByPolicy))
		must(agentsession.NewDispatch("a", call))
		must(agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("a", "charged")))
		must(&agentsession.ResponseEntry{ResponseID: "resp-2", Status: "completed"})
		end(agentsession.ReasonDone)
	})
	// The resume adds a user message and takes up nothing.
	message := write("message", func(must appender, end func(string), _ string) {
		must(agentsession.NewItemEntry(openresponses.UserText("and another thing")))
		must(&agentsession.ResponseEntry{ResponseID: "resp-2", Status: "completed"})
		end(agentsession.ReasonDone)
	})
	for _, tt := range []struct {
		path, note string
	}{
		{refused, fmt.Sprintf(emptyResumeNote, "run-2")},
		{cut, fmt.Sprintf(emptyResumeNote, "run-2")},
		{message, sourceNote},
	} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"verify", tt.path}, &stdout, &stderr); code != 1 {
			t.Errorf("%s: exit %d, want 1\n%s", tt.path, code, stdout.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "run source disagrees with its segment") || !strings.Contains(out, tt.note) {
			t.Errorf("%s: stdout lacks the mismatch or %q:\n%s", tt.path, tt.note, out)
		}
		if tt.note == sourceNote && strings.Contains(out, "took up nothing") {
			t.Errorf("%s: a resume that adds a message is noted as empty:\n%s", tt.path, out)
		}
	}
}

// An appender appends an entry to the session a test builds and
// returns its id.
type appender = func(agentsession.Entry) string

// writeResumed writes a session to path that records a held call and a
// run written resume, then has tail finish it with an appender, an
// ender and the call's entry id. It returns the session's id.
func writeResumed(t *testing.T, path string, tail func(must appender, end func(reason string), call string)) string {
	t.Helper()
	s := agentsession.New(agentsession.Header{})
	must := func(e agentsession.Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	end := func(reason string) {
		t.Helper()
		e, err := s.EndRun(reason, "")
		if err != nil {
			t.Fatal(err)
		}
		must(e)
	}
	must(agentsession.NewRunStart("run-1", agentsession.SourceInput, ""))
	must(agentsession.NewItemEntry(openresponses.UserText("charge it")))
	call := must(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "a", Name: "charge", Arguments: "{}"}, ResponseID: "resp-1"})
	must(&agentsession.ResponseEntry{ResponseID: "resp-1", Status: "completed"})
	must(agentsession.NewDecision("a", call, agentsession.VerdictHold, agentsession.ByPolicy))
	end(agentsession.ReasonInputRequired)
	must(agentsession.NewRunStart("run-2", agentsession.SourceResume, ""))
	tail(must, end, call)
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

// refusedResume ends the resume as a subscriber that refused it does,
// before it took the call up.
func refusedResume(_ appender, end func(string), _ string) {
	end(agentsession.ReasonError)
}

// writeEarly09 writes a 0.9 file to path that repeats a call ID, which
// draft 0.9 forbade only after its first writers shipped, and returns
// the session's id.
func writeEarly09(t *testing.T, path string) string {
	t.Helper()
	const id = "01995b2a-0000-7000-8000-00000000000a"
	lines := []string{`{"type":"session","format":"agentsession/0.9","id":"` + id + `","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`}
	parent := "null"
	for _, body := range []string{
		`"type":"item","item":{"type":"function_call","id":"f1","call_id":"x","name":"t","arguments":"{}"}`,
		`"type":"item","item":{"type":"function_call","id":"f2","call_id":"x","name":"t","arguments":"{}"}`,
	} {
		l := `{` + body + `,"parent":` + parent + `,"ts":"2026-09-17T16:00:01Z"}`
		eid, _, err := agentsession.EntryHashes([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, `{"id":"`+eid+`",`+l[1:])
		parent = `"` + eid + `"`
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

// importFile imports a session file into a cas store and closes the
// store. With declared set, the session's stored header is rewritten to
// declare that format, as a store an earlier release wrote holds it:
// Import writes the header this release's Read raises, and this release
// raises a stored header only when it appends.
func importFile(t *testing.T, root, path, declared string) {
	t.Helper()
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Import(context.Background(), f, true)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if declared == "" {
		return
	}
	hp := filepath.Join(root, "sessions", s.ID(), "header")
	data, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	var h agentsession.Header
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	h.Format = declared
	if data, err = json.Marshal(h); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCASVerifyNotes: verify of a cas root, and of one session a store
// holds, print the notes verify of the session's file does: the note on
// a resume that took up nothing beside the session's error, since it
// names a run, and the note on a 0.9 file of an early writer once at
// the end. The store's header, not the projection's, says what the
// session declared: the projection is a file this release writes and
// declares this release's format (#187).
func TestCASVerifyNotes(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "cas")
	refused := writeResumed(t, filepath.Join(tmp, "refused.jsonl"), refusedResume)
	importFile(t, root, filepath.Join(tmp, "refused.jsonl"), "")
	early := writeEarly09(t, filepath.Join(tmp, "early.jsonl"))
	importFile(t, root, filepath.Join(tmp, "early.jsonl"), "agentsession/0.9")
	// A second 0.9 session, so the note at the end is shown to be
	// printed once for both.
	other := filepath.Join(tmp, "other")
	writeEarly09(t, filepath.Join(tmp, "other.jsonl"))
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	// Import refuses a session the store holds, so the second copy goes
	// into a store of its own and the two are checked apart.
	importFile(t, other, filepath.Join(tmp, "other.jsonl"), "agentsession/0.9")

	emptyNote := fmt.Sprintf(emptyResumeNote, "run-2")
	tests := []struct {
		name   string
		args   []string
		stdout []string
		count  map[string]int
	}{
		{
			name: "the store", args: []string{"verify", root},
			stdout: []string{
				refused + ": records to ", "run source disagrees with its segment",
				refused + ": " + emptyNote,
				early + ": records to ", "call ID repeated",
				"2 sessions' hashes and records checked, 2 failed",
			},
			count: map[string]int{earlyNote: 1, emptyNote: 1},
		},
		{name: "the resumed session", args: []string{"verify", root, refused}, stdout: []string{emptyNote}, count: map[string]int{earlyNote: 0}},
		{name: "the early 0.9 session", args: []string{"verify", root, early}, stdout: []string{"call ID repeated", earlyNote}, count: map[string]int{earlyNote: 1}},
		{name: "the other store", args: []string{"verify", other}, count: map[string]int{earlyNote: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != 1 {
				t.Errorf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
			}
			for _, want := range tt.stdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
				}
			}
			for note, n := range tt.count {
				if got := strings.Count(stdout.String(), note); got != n {
					t.Errorf("stdout has %d of %q, want %d:\n%s", got, note, n, stdout.String())
				}
			}
		})
	}
	// The projection of the early 0.9 session, as a file, declares this
	// release's format and earns no note; the store knows better.
	st, err := cas.Open(root, cas.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	projected, err := st.ProjectDir(context.Background(), filepath.Join(tmp, "projected"), early)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verify", projected}, &stdout, &stderr); code != 1 || strings.Contains(stdout.String(), earlyNote) {
		t.Errorf("verify of the projection: exit %d:\n%s", code, stdout.String())
	}
}

// TestCASVerifyLinks: verify of a cas root, and of one session a store
// holds, checks each subsession link against the header of the session
// it names, and fails on one whose header names another parent or
// another call, which is what a fork remedy that missed a level of
// subsessions leaves; a link to a session the store lacks is a child
// that never started, and passes; a file names no store to find the
// target in, so its links are not checked (#186).
func TestCASVerifyLinks(t *testing.T) {
	ctx := context.Background()
	const parent = "01995b2a-0000-7000-8000-0000000000a1"
	const call = "call_g"
	child := agentsession.SubsessionID(parent, call)
	// build makes a store holding the parent, which links the subsession
	// the call spawned, and the child under the given header, or no
	// child with none.
	build := func(t *testing.T, root string, childHeader *agentsession.Header) {
		t.Helper()
		st, err := cas.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.Create(ctx, agentsession.Header{ID: parent}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, parent, agentsession.NewItemEntry(openresponses.UserText("delegate"))); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, parent, &agentsession.LinkEntry{Rel: agentsession.RelSubsession, Session: child, CallID: call}); err != nil {
			t.Fatal(err)
		}
		if childHeader == nil {
			return
		}
		if _, err := st.Create(ctx, *childHeader); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, child, agentsession.NewItemEntry(openresponses.UserText("do it"))); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		child  *agentsession.Header
		code   int
		stdout []string // on verify of the store and of the parent alike
	}{
		{"the link and the header agree", &agentsession.Header{ID: child, ParentSession: parent, SpawnedBy: call}, 0, nil},
		{"the child never started", nil, 0, nil},
		{"the header names another parent", &agentsession.Header{ID: child, ParentSession: "01995b2a-0000-7000-8000-0000000000b2", SpawnedBy: call}, 1, []string{"link ", " -> " + child + "  ERROR ", "parent_session"}},
		{"the header names another call", &agentsession.Header{ID: child, ParentSession: parent, SpawnedBy: "call_h"}, 1, []string{"link ", " -> " + child + "  ERROR ", "spawned_by"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			build(t, root, tt.child)
			for _, args := range [][]string{{"verify", root}, {"verify", root, parent}} {
				var stdout, stderr bytes.Buffer
				if code := run(args, &stdout, &stderr); code != tt.code {
					t.Errorf("%v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", args, code, tt.code, stdout.String(), stderr.String())
				}
				for _, want := range tt.stdout {
					if !strings.Contains(stdout.String(), want) {
						t.Errorf("%v: stdout lacks %q:\n%s", args, want, stdout.String())
					}
				}
				if len(args) == 2 && tt.code == 1 {
					for _, want := range []string{parent + ": link ", "1 failed"} {
						if !strings.Contains(stdout.String(), want) {
							t.Errorf("%v: stdout lacks %q:\n%s", args, want, stdout.String())
						}
					}
				}
			}
			if tt.code == 0 {
				return
			}
			// The parent's file alone: no store to find the child in,
			// so the link is not checked.
			st, err := cas.Open(root, cas.WithReadOnly())
			if err != nil {
				t.Fatal(err)
			}
			projected, err := st.ProjectDir(ctx, filepath.Join(root, "..", "projected"), parent)
			st.Close()
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"verify", projected}, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "link ") {
				t.Errorf("verify of the file: exit %d:\n%s", code, stdout.String())
			}
		})
	}
}

// TestNoteWriter: no release checks a run's source or end as it is
// appended, so a mismatch earns a note naming the writer of the run in
// a file of any minor, never the note on early 0.9 writers (#165).
func TestNoteWriter(t *testing.T) {
	for err, note := range map[error]string{
		agentsession.ErrSourceMismatch: sourceNote,
		agentsession.ErrReasonMismatch: reasonNote,
	} {
		wrapped := fmt.Errorf("%w: run r", err)
		for declared, want := range map[string]string{
			"agentsession/0.8":  earlierNote + "\n" + note,
			"agentsession/0.9":  note,
			agentsession.Format: note,
		} {
			if got := noteFor(declared, wrapped); got != want {
				t.Errorf("%v in %s: note %q, want %q", err, declared, got, want)
			}
		}
	}
}

// TestShowOriginDispatch: show on a cas fork made at a call lists the
// call pending with the dispatch its origin holds, read through the
// store; the file the fork projects to reads alone and says the call is
// unknown, with no dispatch to show (#185).
func TestShowOriginDispatch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "cas")
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := st.Create(ctx, agentsession.Header{Records: agentsession.AllRecords})
	if err != nil {
		t.Fatal(err)
	}
	call, err := st.Append(ctx, origin.ID(), &agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fk", CallID: "call_k", Name: "deploy", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, origin.ID(), agentsession.NewDispatch("call_k", call).WithIdempotencyKey("k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, origin.ID(), agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_k", "deployed"))); err != nil {
		t.Fatal(err)
	}
	fork, err := st.Create(ctx, agentsession.Header{ParentSession: origin.ID(), Base: call, Records: agentsession.AllRecords})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := st.ProjectDir(ctx, filepath.Join(tmp, "projected"), fork.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		args   []string
		stdout []string
		absent []string
	}{
		{
			name: "through the store", args: []string{"show", root, fork.ID()},
			stdout: []string{"pending at", "call_k  deploy  unknown  dispatched in session " + origin.ID() + ", key \"k1\""},
		},
		{
			name: "the projected file", args: []string{"show", projected},
			stdout: []string{"pending at", "call_k  deploy  unknown"}, absent: []string{"dispatched"},
		},
		{
			name: "the origin", args: []string{"show", root, origin.ID()},
			absent: []string{"pending at"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			for _, want := range tt.stdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(stdout.String(), absent) {
					t.Errorf("stdout has %q:\n%s", absent, stdout.String())
				}
			}
		})
	}
}

// TestCASRepairSalvagedLeaf: a branch leaf whose record is damaged, a
// readable append following it, is salvaged and said so, rather than
// the head reported as the log last named it with nothing about the
// leaf (#189).
func TestCASRepairSalvagedLeaf(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	const id = "leafy"
	if _, err := st.Create(ctx, agentsession.Header{ID: id}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, text := range []string{"one", "two", "three"} {
		eid, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText(text)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, eid)
	}
	if err := st.SetHead(ctx, id, ids[2], ids[1]); err != nil {
		t.Fatal(err)
	}
	four, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("four, from two")))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Three's record, damaged in its session member, so both hashes it
	// spells stay legible.
	path := filepath.Join(root, "sessions", id, "log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	for i, l := range lines {
		if strings.Contains(l, `"op":"append"`) && strings.Contains(l, ids[2]) {
			b := []byte(l)
			b[strings.Index(l, `"session":"`)+len(`"session":"`)] ^= 1
			lines[i] = string(b)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"repair", root, id, "-dry-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, w := range []string{
		"kept     4 entries, 1 of them recovered from what the damage hid",
		"salvaged " + shortID(ids[2]) + ": named only by a damaged record, and its objects whole; not the head",
		"head     " + shortID(four) + ", as the log last named it",
	} {
		if !strings.Contains(stdout.String(), w) {
			t.Errorf("stdout lacks %q:\n%s", w, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "dropped") {
		t.Errorf("a leaf the damage spelled whole is dropped:\n%s", stdout.String())
	}
}
