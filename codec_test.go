package agentsession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestRoundTripFixtures reads every positive fixture and writes it back.
// The output must be byte-identical: that covers unknown entry types,
// namespaced items, unknown members on known entries and unknown header
// fields, all of which the format requires a reader to preserve.
func TestRoundTripFixtures(t *testing.T) {
	for _, name := range []string{"basic", "compaction", "branch", "extensions", "runs", "interleaved", "instructions", "queued", "resume", "pinned"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "sessions", name+".jsonl")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Read(bytes.NewReader(want))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if s.Truncated() != nil {
				t.Fatalf("unexpected truncation: %v", s.Truncated())
			}
			var got bytes.Buffer
			if err := Write(&got, s); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if !bytes.Equal(want, got.Bytes()) {
				t.Errorf("round trip changed the file\nwant:\n%s\ngot:\n%s", want, got.Bytes())
			}
			// Reading the output again must give the same tree.
			again, err := Read(bytes.NewReader(got.Bytes()))
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if again.Len() != s.Len() || again.Leaf() != s.Leaf() {
				t.Errorf("re-read: %d entries leaf %s, want %d leaf %s", again.Len(), again.Leaf(), s.Len(), s.Leaf())
			}
		})
	}
}

func TestReadExtensions(t *testing.T) {
	s := loadFixture(t, "extensions")
	h := s.Header()
	if got := string(h.Extra["acme:workspace"]); got != `"ws-1"` {
		t.Errorf("header extra = %q", got)
	}
	if h.Media != MediaInline {
		t.Errorf("media = %q", h.Media)
	}
	e, _ := s.Entry("c0000001")
	cfg, ok := e.(*ConfigEntry)
	if !ok {
		t.Fatalf("c0000001 = %T", e)
	}
	if got := string(cfg.Unknown["acme_note"]); got != `"kept on a known entry"` {
		t.Errorf("unknown member on config = %q", got)
	}
	e, _ = s.Entry("n0000001")
	u, ok := e.(*UnknownEntry)
	if !ok {
		t.Fatalf("n0000001 = %T", e)
	}
	if u.Type != "agentturn:note" || u.Parent != "i0000001" || u.Timestamp.IsZero() {
		t.Errorf("unknown entry = %+v", u)
	}
	if !IsExtension(u.Type) || IsExtension(TypeItem) {
		t.Error("IsExtension")
	}
	e, _ = s.Entry("i0000002")
	item := e.(*ItemEntry)
	ui, ok := item.Item.(*openresponses.UnknownItem)
	if !ok || ui.Type != "agentturn:note" || ui.ID != "nt_1" {
		t.Errorf("namespaced item = %#v", item.Item)
	}
	if item.IsVisible() {
		t.Error("visible:false item reported visible")
	}
	e, _ = s.Entry("u0000001")
	if c := e.(*CustomEntry); c.NS != "acme" || string(c.Data) != `{"k":"v","n":[1,2,3]}` {
		t.Errorf("custom = %+v", c)
	}
	e, _ = s.Entry("k0000001")
	if l := e.(*LinkEntry); l.Rel != RelSubsession || l.CallID != "call_9" {
		t.Errorf("link = %+v", l)
	}
	e, _ = s.Entry("e0000001")
	env := e.(*EnvEntry)
	if env.VCS == nil || env.VCS.Revision != "abc123" || !env.VCS.Dirty || env.Files.Written["main.go"] != "sha256:bbbb" || env.Tools["go"] != "1.25.0" {
		t.Errorf("env = %+v", env)
	}
	e, _ = s.Entry("r0000002")
	if r := e.(*ResponseEntry); r.Status != openresponses.ResponseStatusFailed || r.Error == nil || r.Error.Code != "upstream" {
		t.Errorf("failed response = %+v", r)
	}
}

func TestReadTruncated(t *testing.T) {
	s := loadFixture(t, "truncated")
	tr := s.Truncated()
	if tr == nil {
		t.Fatal("truncated line not reported")
	}
	if tr.Line != 4 || !strings.HasPrefix(string(tr.Data), `{"type":"item","id":"i0000002"`) {
		t.Errorf("truncated = line %d data %q", tr.Line, tr.Data)
	}
	if tr.Err == nil || tr.Error() == "" || errors.Unwrap(tr) != tr.Err {
		t.Errorf("truncated error = %v", tr.Err)
	}
	if s.Len() != 2 || s.Leaf() != "i0000001" {
		t.Errorf("entries %d leaf %s; the intact prefix should load", s.Len(), s.Leaf())
	}
	// Writing the session back drops the broken line.
	var out bytes.Buffer
	if err := Write(&out, s); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 3 {
		t.Errorf("wrote %d lines, want 3", strings.Count(out.String(), "\n"))
	}
}

func TestReadErrors(t *testing.T) {
	tests := []struct {
		name string
		want error  // sentinel, when there is one
		text string // substring of the message otherwise
	}{
		{name: "bad-parent", want: ErrNoEntry},
		{name: "duplicate-id", want: ErrDuplicateEntry},
		{name: "unsupported-format", want: ErrUnsupportedFormat},
		{name: "corrupt-middle", text: "line 3"},
		{name: "missing-id", text: "no id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", "sessions", tt.name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			_, err = Read(f)
			if err == nil {
				t.Fatal("Read succeeded")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.text != "" && !strings.Contains(err.Error(), tt.text) {
				t.Errorf("err = %v, want substring %q", err, tt.text)
			}
		})
	}
	for name, in := range map[string]string{
		"empty":             "",
		"no header":         `{"type":"item","id":"a","parent":null,"ts":"2026-09-17T16:00:00Z","item":{"type":"message","role":"user","content":"x"}}` + "\n",
		"header not json":   "{\n",
		"header no id":      `{"type":"session","format":"agentsession/0.1","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}` + "\n",
		"unknown payload":   `{"type":"session","format":"agentsession/0.1","id":"x","created_at":"2026-09-17T16:00:00Z","payload":"anthropic/2026"}` + "\n",
		"format not ours":   `{"type":"session","format":"pi/1","id":"x","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}` + "\n",
		"header wrong type": `{"type":"item","format":"agentsession/0.1","id":"x","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(strings.NewReader(in)); err == nil {
				t.Error("Read succeeded")
			}
		})
	}
}

// TestReadMinorVersion checks that a later minor version of the format
// is accepted and that the header keeps its version on rewrite.
func TestReadMinorVersion(t *testing.T) {
	in := `{"type":"session","format":"agentsession/0.7","id":"x","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24","future_field":{"a":1}}` + "\n"
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Write(&out, s); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Errorf("rewrite changed header\nwant %s\ngot  %s", in, out.String())
	}
}

func TestReadCRLFAndBlankLines(t *testing.T) {
	in := "{\"type\":\"session\",\"format\":\"agentsession/0.1\",\"id\":\"x\",\"created_at\":\"2026-09-17T16:00:00Z\",\"payload\":\"openresponses/2026-04-24\"}\r\n\r\n{\"type\":\"info\",\"id\":\"a\",\"parent\":null,\"ts\":\"2026-09-17T16:00:00Z\",\"name\":\"n\"}\r\n"
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Name() != "n" {
		t.Errorf("entries %d name %q", s.Len(), s.Name())
	}
}
