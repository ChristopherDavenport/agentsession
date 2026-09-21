package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
