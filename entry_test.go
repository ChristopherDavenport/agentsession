package agentsession

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// TestEntryEncoding checks the wire form of every entry type: envelope
// first in a fixed order, then the type's members, then unknown members
// in key order, and that decoding the output yields the same value.
func TestEntryEncoding(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 1, 500_000_000, time.UTC)
	base := func(id, parent string) EntryBase { return EntryBase{ID: id, Parent: parent, Timestamp: ts} }
	low := openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortLow}
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{
			name:  "item root",
			entry: &ItemEntry{EntryBase: base("a", ""), Item: openresponses.UserText("hi")},
			want:  `{"type":"item","id":"a","parent":null,"ts":"2026-09-17T12:00:01.5Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`,
		},
		{
			name:  "item with response and hidden",
			entry: &ItemEntry{EntryBase: base("b", "a"), Item: openresponses.AssistantText("yo"), ResponseID: "resp_1", Visible: ptr(false)},
			want:  `{"type":"item","id":"b","parent":"a","ts":"2026-09-17T12:00:01.5Z","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo","annotations":[]}]},"response":"resp_1","visible":false}`,
		},
		{
			name: "item with unknown members",
			entry: &ItemEntry{EntryBase: EntryBase{ID: "c", Parent: "b", Timestamp: ts,
				Unknown: map[string]json.RawMessage{"z": json.RawMessage(`1`), "a": json.RawMessage(`{"k":"<v>"}`)}},
				Item: openresponses.UserText("hi")},
			want: `{"type":"item","id":"c","parent":"b","ts":"2026-09-17T12:00:01.5Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},"a":{"k":"<v>"},"z":1}`,
		},
		{
			name: "response",
			entry: &ResponseEntry{EntryBase: base("r", "b"), ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusIncomplete,
				Usage:       &openresponses.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
				Incomplete:  &openresponses.IncompleteDetails{Reason: openresponses.IncompleteReasonMaxOutputTokens},
				RequestHash: "sha256:ab", LatencyMS: 9},
			want: `{"type":"response","id":"r","parent":"b","ts":"2026-09-17T12:00:01.5Z","response_id":"resp_1","model":"m","status":"incomplete","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"incomplete":{"reason":"max_output_tokens"},"request_hash":"sha256:ab","latency_ms":9}`,
		},
		{
			name: "config",
			entry: &ConfigEntry{EntryBase: base("c", ""), Model: "m", Instructions: ptr("i"), Reasoning: &low,
				Text:         &openresponses.TextConfig{Format: &openresponses.TextFormat{Type: "json_object"}},
				ToolsAdded:   openresponses.Tools{openresponses.NewFunctionTool("f", "d", json.RawMessage(`{"type":"object"}`))},
				ToolsRemoved: []string{"g"}, Extra: map[string]json.RawMessage{"temperature": json.RawMessage("0.1")}, Replace: true},
			want: `{"type":"config","id":"c","parent":null,"ts":"2026-09-17T12:00:01.5Z","model":"m","instructions":"i","reasoning":{"effort":"low","summary":null},"text":{"format":{"type":"json_object"}},"tools_added":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],"tools_removed":["g"],"extra":{"temperature":0.1},"replace":true}`,
		},
		{
			name:  "config empty delta",
			entry: &ConfigEntry{EntryBase: base("c", "")},
			want:  `{"type":"config","id":"c","parent":null,"ts":"2026-09-17T12:00:01.5Z"}`,
		},
		{
			name: "compaction with server item",
			entry: &CompactionEntry{EntryBase: base("k", "c"), FirstKept: "a", Summary: &openresponses.Compaction{ID: "cmp_1", EncryptedContent: "xyz"},
				Config: Settings{Model: "m", Tools: openresponses.Tools{openresponses.NewFunctionTool("f", "", nil)}}, TokensBefore: 10,
				Usage: &openresponses.Usage{InputTokens: 5, TotalTokens: 5}},
			want: `{"type":"compaction","id":"k","parent":"c","ts":"2026-09-17T12:00:01.5Z","first_kept":"a","summary":{"type":"compaction","id":"cmp_1","encrypted_content":"xyz"},"config":{"model":"m","tools":[{"type":"function","name":"f","description":"","parameters":null}]},"tokens_before":10,"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`,
		},
		{
			name:  "branch summary",
			entry: &BranchSummaryEntry{EntryBase: base("s", "a"), From: "z", Summary: openresponses.SystemText("left")},
			want:  `{"type":"branch_summary","id":"s","parent":"a","ts":"2026-09-17T12:00:01.5Z","from":"z","summary":{"type":"message","role":"system","content":[{"type":"input_text","text":"left"}]}}`,
		},
		{
			name:  "label set",
			entry: &LabelEntry{EntryBase: base("l", "a"), Target: "a", Label: ptr("checkpoint")},
			want:  `{"type":"label","id":"l","parent":"a","ts":"2026-09-17T12:00:01.5Z","target":"a","label":"checkpoint"}`,
		},
		{
			name:  "label clear",
			entry: &LabelEntry{EntryBase: base("l", "a"), Target: "a"},
			want:  `{"type":"label","id":"l","parent":"a","ts":"2026-09-17T12:00:01.5Z","target":"a","label":null}`,
		},
		{
			name:  "info",
			entry: &InfoEntry{EntryBase: base("n", "a"), Name: "Refactor auth"},
			want:  `{"type":"info","id":"n","parent":"a","ts":"2026-09-17T12:00:01.5Z","name":"Refactor auth"}`,
		},
		{
			name: "item converging a local worker and a subagent leaf",
			entry: &ItemEntry{EntryBase: EntryBase{ID: "j", Parent: "a", Timestamp: ts,
				Parents: []EntryRef{{Entry: "w7"}, {Session: "01J", Entry: "c4"}}},
				Item: openresponses.UserText("merged")},
			want: `{"type":"item","id":"j","parent":"a","parents":[{"entry":"w7"},{"session":"01J","entry":"c4"}],"ts":"2026-09-17T12:00:01.5Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"merged"}]}}`,
		},
		{
			name: "env",
			entry: &EnvEntry{EntryBase: base("e", "a"), CWD: "/p", VCS: &VCS{System: "git", Revision: "abc", Dirty: true},
				Files: &FileHashes{Read: map[string]string{"a": "sha256:1"}}, Tools: map[string]string{"go": "1.25"}},
			want: `{"type":"env","id":"e","parent":"a","ts":"2026-09-17T12:00:01.5Z","cwd":"/p","vcs":{"system":"git","revision":"abc","dirty":true},"files":{"read":{"a":"sha256:1"}},"tools":{"go":"1.25"}}`,
		},
		{
			name:  "outcome",
			entry: &OutcomeEntry{EntryBase: base("o", "a"), Kind: OutcomeTest, Target: "a", Score: ptr(0.5), Label: "3/6", Details: json.RawMessage(`{"failed":["x"]}`)},
			want:  `{"type":"outcome","id":"o","parent":"a","ts":"2026-09-17T12:00:01.5Z","kind":"test","target":"a","score":0.5,"label":"3/6","details":{"failed":["x"]}}`,
		},
		{
			name:  "link",
			entry: &LinkEntry{EntryBase: base("k", "a"), Rel: RelForkOf, Session: "s2"},
			want:  `{"type":"link","id":"k","parent":"a","ts":"2026-09-17T12:00:01.5Z","rel":"fork_of","session":"s2"}`,
		},
		{
			name:  "custom",
			entry: &CustomEntry{EntryBase: base("u", "a"), NS: "acme", Data: json.RawMessage(`[1,"<two>"]`)},
			want:  `{"type":"custom","id":"u","parent":"a","ts":"2026-09-17T12:00:01.5Z","ns":"acme","data":[1,"<two>"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MarshalEntry(tt.entry)
			if err != nil {
				t.Fatalf("MarshalEntry: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("MarshalEntry =\n%s\nwant\n%s", got, tt.want)
			}
			back, err := UnmarshalEntry(got)
			if err != nil {
				t.Fatalf("UnmarshalEntry: %v", err)
			}
			if back.EntryType() != tt.entry.EntryType() {
				t.Errorf("type = %s, want %s", back.EntryType(), tt.entry.EntryType())
			}
			again, err := MarshalEntry(back)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != tt.want {
				t.Errorf("second encoding differs\n%s\nwant\n%s", again, tt.want)
			}
			if back.Base().ID != tt.entry.Base().ID || back.Base().Parent != tt.entry.Base().Parent || !back.Base().Timestamp.Equal(ts) {
				t.Errorf("envelope = %+v", back.Base())
			}
		})
	}
}

func TestUnmarshalEntryErrors(t *testing.T) {
	tests := map[string]string{
		"not json":           `{`,
		"no type":            `{"id":"a","parent":null,"ts":"2026-09-17T12:00:00Z"}`,
		"no id":              `{"type":"info","parent":null,"ts":"2026-09-17T12:00:00Z"}`,
		"no ts":              `{"type":"info","id":"a","parent":null}`,
		"bad ts":             `{"type":"info","id":"a","parent":null,"ts":"yesterday"}`,
		"item missing":       `{"type":"item","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z"}`,
		"item null":          `{"type":"item","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z","item":null}`,
		"item not object":    `{"type":"item","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z","item":3}`,
		"compaction no sum":  `{"type":"compaction","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z","first_kept":"x","config":{}}`,
		"branch no summary":  `{"type":"branch_summary","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z","from":"x"}`,
		"config bad tools":   `{"type":"config","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z","tools_added":[3]}`,
		"response bad usage": `{"type":"response","id":"a","parent":null,"ts":"2026-09-17T12:00:00Z","response_id":"r","status":"completed","usage":"lots"}`,
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := UnmarshalEntry([]byte(in)); err == nil {
				t.Errorf("UnmarshalEntry accepted %s", in)
			}
		})
	}
}

func TestMarshalEntryErrors(t *testing.T) {
	for name, e := range map[string]Entry{
		"nil":                 nil,
		"item without item":   &ItemEntry{},
		"compaction no sum":   &CompactionEntry{},
		"branch no summary":   &BranchSummaryEntry{},
		"unknown without raw": &UnknownEntry{Type: "x:y"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := MarshalEntry(e); err == nil {
				t.Error("MarshalEntry succeeded")
			}
		})
	}
}

func TestUnknownEntryPreservesBytes(t *testing.T) {
	line := `{"type":"acme:thing","id":"u1","parent":"p","ts":"2026-09-17T12:00:00Z","z":1,"a":"<&>","nested":{"q":[1,{"b":null}]}}`
	e, err := UnmarshalEntry([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	u, ok := e.(*UnknownEntry)
	if !ok {
		t.Fatalf("got %T", e)
	}
	if u.Type != "acme:thing" || u.ID != "u1" || u.Parent != "p" || InContext(u) {
		t.Errorf("unknown = %+v", u)
	}
	out, err := MarshalEntry(u)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != line {
		t.Errorf("re-emitted\n%s\nwant\n%s", out, line)
	}
	// Even a core type name that this version does not know, without a
	// namespace, is preserved rather than rejected.
	future := `{"type":"checkpoint","id":"f1","parent":null,"ts":"2026-09-17T12:00:00Z","blob":"..."}`
	e, err = UnmarshalEntry([]byte(future))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*UnknownEntry); !ok {
		t.Errorf("future type decoded as %T", e)
	}
}

func TestInContext(t *testing.T) {
	for _, tt := range []struct {
		e    Entry
		want bool
	}{
		{&ItemEntry{}, true},
		{&BranchSummaryEntry{}, true},
		{&CompactionEntry{}, true},
		{&ResponseEntry{}, false},
		{&ConfigEntry{}, false},
		{&LabelEntry{}, false},
		{&InfoEntry{}, false},
		{&EnvEntry{}, false},
		{&OutcomeEntry{}, false},
		{&LinkEntry{}, false},
		{&CustomEntry{}, false},
		{&UnknownEntry{}, false},
	} {
		if got := InContext(tt.e); got != tt.want {
			t.Errorf("InContext(%T) = %v", tt.e, got)
		}
	}
}

func TestNewItemEntry(t *testing.T) {
	e := NewItemEntry(openresponses.UserText("x"))
	if !e.IsVisible() || e.Item.ItemType() != "message" || e.EntryType() != TypeItem {
		t.Errorf("NewItemEntry = %+v", e)
	}
	if !strings.HasPrefix(TypeItem, "item") {
		t.Fatal("unreachable")
	}
}
