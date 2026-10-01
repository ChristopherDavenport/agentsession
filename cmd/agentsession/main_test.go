package main

import (
	"bytes"
	"context"
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
