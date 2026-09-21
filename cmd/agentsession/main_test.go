package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

const fixtures = "../../testdata/sessions"

func TestRun(t *testing.T) {
	tmp := t.TempDir()
	tampered := filepath.Join(tmp, "tampered.jsonl")
	data, err := os.ReadFile(filepath.Join(fixtures, "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tampered, bytes.ReplaceAll(data, []byte(`"request_hash":"sha256:`), []byte(`"request_hash":"sha256:0`)), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session whose responses recorded no hash. Verify reports each
	// with ErrNoHash; the CLI counts those apart from the verified and
	// the failed alike, and does not fail the run over them.
	unhashed := filepath.Join(tmp, "unhashed.jsonl")
	stripped := regexp.MustCompile(`,"request_hash":"sha256:[0-9a-f]+"`).ReplaceAll(data, nil)
	if err := os.WriteFile(unhashed, stripped, 0o600); err != nil {
		t.Fatal(err)
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
		{name: "show too many", args: []string{"show", "a", "b"}, code: 2, stderr: []string{"expected one session file, got 2"}},
		{
			name: "show", args: []string{"show", filepath.Join(fixtures, "branch.jsonl")},
			stdout: []string{"name     Branching demo", "fork×2 [fork]", "r0000002  i0000004  response", "leaf", "context at n0000001", `system: "An earlier attempt`, `user: "follow-up B"`},
			absent: []string{"truncated"},
		},
		{
			name: "show at leaf", args: []string{"show", "-leaf", "r0000002", filepath.Join(fixtures, "branch.jsonl")},
			stdout: []string{"context at r0000002", `user: "follow-up A"`},
			absent: []string{`    4  user: "follow-up B"`},
		},
		{name: "show bad leaf", args: []string{"show", filepath.Join(fixtures, "branch.jsonl"), "-leaf", "zz"}, code: 1, stderr: []string{"no such entry"}},
		{
			name: "show compaction", args: []string{"show", filepath.Join(fixtures, "compaction.jsonl")},
			stdout: []string{"compaction", "first kept", "replace, model gpt-5-nano"},
		},
		{
			name: "show truncated", args: []string{"show", filepath.Join(fixtures, "truncated.jsonl")},
			stdout: []string{"truncated: line 4"},
		},
		{name: "verify", args: []string{"verify", filepath.Join(fixtures, "branch.jsonl")}, stdout: []string{"r0000001  ok", "3 verified, 0 without hash, 0 failed"}},
		{name: "verify mismatch", args: []string{"verify", tampered}, code: 1, stdout: []string{"MISMATCH", "2 failed"}},
		{
			name: "verify without a recorded hash", args: []string{"verify", unhashed},
			stdout: []string{"r0000001  no hash recorded", "0 verified, 2 without hash, 0 failed"},
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
				"first kept i0000004", "1 pinned",
				`1  system: "Summary: the user said first`,
				`2  developer: "House rule: never use Box::leak."`,
			},
		},
		{name: "verify truncated", args: []string{"verify", filepath.Join(fixtures, "truncated.jsonl")}, code: 1, stdout: []string{"truncated: line 4"}},
		{name: "verify runs", args: []string{"verify", filepath.Join(fixtures, "runs.jsonl")}, stdout: []string{"2 verified, 0 without hash, 0 failed"}, absent: []string{"records to"}},
		{name: "verify bad records", args: []string{"verify", filepath.Join(fixtures, "bad-records.jsonl")}, code: 1, stdout: []string{"records to i0000003  ERROR", "call call_1 has an output and no dispatch"}},
		{
			name: "show runs", args: []string{"show", filepath.Join(fixtures, "runs.jsonl")},
			stdout: []string{"records  run, dispatch, decision", "start run-1 input ref", "hold call_1 by policy", "call_2 to tool", "reject call_3 by policy", "end run-1 input_required pending call_1", "end run-2 done", "eval on r0000002 score 0.9 pass", "container sha256:9f2c1e4b7a0d…"},
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
			stdout: []string{`agentpolicy {"verdict":"allow","rule":"bash(ls:*)"}`},
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
			stdout:  []string{filepath.Join(out, "01995b2a-0000-7000-8000-000000000003.json"), "_r0000002.json"},
			written: []string{filepath.Join(out, "01995b2a-0000-7000-8000-000000000003.json"), filepath.Join(out, "01995b2a-0000-7000-8000-000000000003_r0000002.json")},
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
		{entry: &agentsession.CompactionEntry{FirstKept: "i1", Summary: openresponses.UserText("so far")}, want: "first kept i1"},
		{entry: &agentsession.BranchSummaryEntry{From: "i1", Summary: openresponses.UserText("before")}, want: "from i1"},
		{entry: agentsession.NewRunStart("run-1", agentsession.SourceInput, "cron:x"), want: "start run-1 input"},
		{entry: agentsession.NewRunEnd("run-1", agentsession.ReasonStopped, "max_turns", nil), want: "end run-1 stopped"},
		{entry: agentsession.NewDispatch("call_1", "i2"), want: "call_1 to tool"},
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
