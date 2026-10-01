package agentsession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/openresponses"
)

// scanAll runs Scan to the end, returning the ids of the entries it
// yielded once each and the error that ended it.
func scanAll(data []byte) ([]string, error) {
	_, seq, err := Scan(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var ids []string
	for e, err := range seq {
		if err != nil {
			var tl *TruncatedLine
			if errors.As(err, &tl) {
				return ids, nil // Read keeps what came before a cut line
			}
			return ids, err
		}
		if !e.Repeat {
			ids = append(ids, e.ID)
		}
	}
	return ids, nil
}

// agree holds Scan to Read's verdict on data: a file Read takes, Scan
// takes with the same ids in the same order; a file Scan refuses, Read
// refuses. Scan may take a file Read refuses only for what it does not
// check, what a core entry type requires of its own members.
func agree(t *testing.T, name string, data []byte) {
	t.Helper()
	ids, serr := scanAll(data)
	s, rerr := Read(bytes.NewReader(data))
	if errors.Is(serr, ErrScanMigrated) {
		return
	}
	var member *memberError
	switch {
	case rerr == nil && serr != nil:
		t.Errorf("%s: Read takes the file, Scan refuses it: %v", name, serr)
	case rerr == nil:
		var want []string
		for _, e := range s.Entries() {
			want = append(want, e.Base().ID)
		}
		if strings.Join(ids, " ") != strings.Join(want, " ") {
			t.Errorf("%s: Scan yields %d ids, Read %d, or in another order", name, len(ids), len(want))
		}
	case serr == nil && !errors.As(rerr, &member):
		t.Errorf("%s: Scan takes the file, Read refuses it: %v", name, rerr)
	}
}

// TestScanAgreesWithRead: on every session fixture, and on each with
// one byte of one entry line changed, Scan reaches Read's verdict on
// the ids and links, without decoding (#127).
func TestScanAgreesWithRead(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "sessions", "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		agree(t, name, data)
		lines := bytes.Split(data, []byte("\n"))
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) < 2 {
				continue
			}
			for _, at := range []int{len(lines[i]) / 3, len(lines[i]) / 2, len(lines[i]) - 2} {
				broken := append([][]byte(nil), lines...)
				l := append([]byte(nil), lines[i]...)
				l[at] ^= 0x01
				broken[i] = l
				agree(t, name+" broken", bytes.Join(broken, []byte("\n")))
			}
		}
	}
}

// scanFile is a session of two entries, r1 and r2 under it, which a
// test line goes under.
type scanFile struct {
	head, sid, r1, r2 string
	lines             []string
}

func newScanFile(t *testing.T) scanFile {
	t.Helper()
	s := New(Header{})
	r1, err := s.Append(NewItemEntry(openresponses.UserText("one")))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Append(NewItemEntry(openresponses.UserText("two")))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	return scanFile{head: lines[0], sid: s.ID(), r1: r1, r2: r2, lines: lines[1:]}
}

// line returns members as an entry line under r2, unless members name
// a parent, with its id the hash of the line as written.
func (f scanFile) line(t *testing.T, members string) string {
	t.Helper()
	members = f.expand(members)
	line := `{` + members + `,"ts":"2026-09-17T16:00:01Z"`
	if !strings.Contains(members, `"parent":`) {
		line += `,"parent":"` + f.r2 + `"`
	}
	line += `}`
	id, _, err := EntryHashes([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return `{"id":"` + id + `",` + line[1:]
}

// expand puts the file's ids in for R1, R2 and SID.
func (f scanFile) expand(s string) string {
	return strings.NewReplacer("R1", f.r1, "R2", f.r2, "SID", f.sid).Replace(s)
}

func (f scanFile) with(lines ...string) []byte {
	all := append([]string{f.head}, f.lines...)
	return []byte(strings.Join(append(all, lines...), "\n") + "\n")
}

// TestScanReadHashTheLine: the id is the hash of the line as written,
// so a line both readers take is one whose members the fields write
// back otherwise, and it is written back as read through Write, Read
// and Append, under the same id; and the readers refuse alike what the
// envelope and the convergence rules refuse (#127).
func TestScanReadHashTheLine(t *testing.T) {
	f := newScanFile(t)
	for name, members := range map[string]string{
		"link without session":           `"type":"link","rel":"subagent"`,
		"custom without ns":              `"type":"custom","data":{"a":1}`,
		"outcome without kind":           `"type":"outcome","target":"R1"`,
		"label without target":           `"type":"label","label":"x"`,
		"bare response":                  `"type":"response"`,
		"env with an empty vcs":          `"type":"env","cwd":"/w","vcs":{}`,
		"item content a string":          `"type":"item","item":{"type":"message","role":"user","content":"hi"}`,
		"parents in another case beside": `"type":"info","name":"n","parents":[{"entry":"R1","Entry":"x","SESSION":"o"}]`,
		"parents into another session":   `"type":"info","name":"n","parents":[{"entry":"sha256:elsewhere","session":"o"}]`,
		"parents an empty list":          `"type":"info","name":"n","parents":[]`,
	} {
		t.Run("taken: "+name, func(t *testing.T) {
			line := f.line(t, members)
			data := f.with(line)
			agree(t, name, data)
			s, err := Read(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if _, err := scanAll(data); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			e := s.Entries()[len(s.Entries())-1]
			id := e.Base().ID
			want, err := jcs.Transform([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			got, err := MarshalEntry(e)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := jcs.Transform(got); !bytes.Equal(got, want) {
				t.Errorf("MarshalEntry changed the line\nread  %s\nwrote %s", want, got)
			}
			// Write, then Read.
			var buf bytes.Buffer
			if err := Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			again, err := Read(&buf)
			if err != nil {
				t.Fatalf("Write then Read: %v", err)
			}
			if _, ok := again.Entry(id); !ok {
				t.Error("Write then Read changed the id")
			}
			// Appended to a session of its own, entry by entry, as a
			// store appends: the id given is the id computed.
			other := New(s.Header())
			for _, e := range s.Entries() {
				if got, err := other.Append(e); err != nil || got != e.Base().ID {
					t.Fatalf("Append = %s, %v; want %s", got, err, e.Base().ID)
				}
			}
			// A caller's change to a member drops the kept line.
			if l, ok := e.(*LabelEntry); ok {
				changed := *l
				changed.Target = f.r1
				out, err := MarshalEntry(&changed)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(out, []byte(`"target":"`+f.r1+`"`)) {
					t.Errorf("a changed target is not written: %s", out)
				}
			}
		})
	}
	for name, tt := range map[string]struct {
		members string
		want    error
	}{
		"parent the empty string":          {`"type":"info","name":"n","parent":""`, ErrBadID},
		"parents Entry":                    {`"type":"info","name":"n","parents":[{"Entry":"R1"}]`, ErrBadConvergence},
		"parents ENTRY":                    {`"type":"info","name":"n","parents":[{"ENTRY":"R1"}]`, ErrBadConvergence},
		"parents session, no entry":        {`"type":"info","name":"n","parents":[{"session":"o"}]`, ErrBadConvergence},
		"parents entry null":               {`"type":"info","name":"n","parents":[{"entry":null,"session":"o"}]`, ErrBadConvergence},
		"parents entry empty":              {`"type":"info","name":"n","parents":[{"entry":"","session":"o"}]`, ErrBadConvergence},
		"parents entry not a string":       {`"type":"info","name":"n","parents":[{"entry":5}]`, ErrBadConvergence},
		"parents twice":                    {`"type":"info","name":"n","parents":[{"entry":"R1"},{"entry":"R1"}]`, ErrBadConvergence},
		"parents twice, own session":       {`"type":"info","name":"n","parents":[{"entry":"R1"},{"entry":"R1","session":"SID"}]`, ErrBadConvergence},
		"parents own parent":               {`"type":"info","name":"n","parents":[{"entry":"R2"}]`, ErrBadConvergence},
		"parents own parent, session":      {`"type":"info","name":"n","parents":[{"entry":"R2","session":"SID"}]`, ErrBadConvergence},
		"parents Session":                  {`"type":"info","name":"n","parents":[{"entry":"sha256:nothere","Session":"o"}]`, ErrBadConvergence},
		"parents not yet in session":       {`"type":"info","name":"n","parents":[{"entry":"sha256:nothere"}]`, ErrBadConvergence},
		"parents session not a string":     {`"type":"info","name":"n","parents":[{"entry":"R1","session":5}]`, nil},
		"parents an element not an object": {`"type":"info","name":"n","parents":["R1"]`, nil},
		"content":                          {`"type":"info","name":"n","content":"x"`, ErrReservedMember},
		"legacy_id a number":               {`"type":"info","name":"n","legacy_id":5`, nil},
		"legacy_id on an extension":        {`"type":"acme:x","legacy_id":5`, nil},
		"normalised on an extension":       {`"type":"acme:x","normalised":"x"`, nil},
		"parent not yet in the file":       {`"type":"info","name":"n","parent":"sha256:nothere"`, ErrNoEntry},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			data := f.with(f.line(t, tt.members))
			agree(t, name, data)
			_, serr := scanAll(data)
			_, rerr := Read(bytes.NewReader(data))
			if serr == nil || rerr == nil {
				t.Fatalf("Scan %v, Read %v; want both to refuse", serr, rerr)
			}
			if tt.want != nil && (!errors.Is(serr, tt.want) || !errors.Is(rerr, tt.want)) {
				t.Errorf("Scan %v, Read %v; want %v", serr, rerr, tt.want)
			}
		})
	}
	// The other way: an id computed over what a reader's fields encode
	// rather than over the line verifies for neither reader.
	t.Run("refused: id over the fields", func(t *testing.T) {
		withSession := f.line(t, `"type":"link","rel":"subagent","session":""`)
		id := withSession[len(`{"id":"`):strings.Index(withSession, `",`)]
		line := `{"id":"` + id + `","type":"link","rel":"subagent","parent":"` + f.r2 + `","ts":"2026-09-17T16:00:01Z"}`
		data := f.with(line)
		agree(t, "id over the fields", data)
		_, serr := scanAll(data)
		_, rerr := Read(bytes.NewReader(data))
		if !errors.Is(serr, ErrBadID) || !errors.Is(rerr, ErrBadID) {
			t.Errorf("Scan %v, Read %v; want ErrBadID", serr, rerr)
		}
	})
}

// TestScanLines: Scan numbers lines and skips empty ones as Read does,
// and a line is the last only when nothing at all follows it.
func TestScanLines(t *testing.T) {
	f := newScanFile(t)
	all := append([]string{f.head}, f.lines...)
	cut := f.lines[1][:len(f.lines[1])/2]
	for name, tt := range map[string]struct {
		data string
		// line is the line both readers report, 0 for a file both take.
		line string
		cut  bool
	}{
		"empty lines":           {all[0] + "\n\n" + all[1] + "\r\n\n" + all[2] + "\n\n", "", false},
		"a line of spaces":      {all[0] + "\n\n" + all[1] + "\n \n" + all[2] + "\n", "line 4", false},
		"spaces last":           {all[0] + "\n" + all[1] + "\n" + all[2] + "\n  ", "", true},
		"cut last":              {all[0] + "\n" + all[1] + "\n" + cut, "", true},
		"cut last, newline":     {all[0] + "\n" + all[1] + "\n" + cut + "\n", "", true},
		"cut, then empty line":  {all[0] + "\n" + all[1] + "\n" + cut + "\n\n", "line 3", false},
		"cut, then spaces":      {all[0] + "\n" + all[1] + "\n" + cut + "\n  ", "line 3", false},
		"cut after empty lines": {all[0] + "\n\n\n" + all[1] + "\n" + cut, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte(tt.data)
			agree(t, name, data)
			s, rerr := Read(bytes.NewReader(data))
			_, seq, err := Scan(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			var serr error
			for _, err := range seq {
				serr = err
			}
			var tl *TruncatedLine
			switch {
			case tt.line != "":
				if rerr == nil || serr == nil || !strings.Contains(rerr.Error(), tt.line+":") || !strings.Contains(serr.Error(), tt.line+":") {
					t.Errorf("Read %v, Scan %v; want both at %s", rerr, serr, tt.line)
				}
			case tt.cut:
				if rerr != nil || s.Truncated() == nil || !errors.As(serr, &tl) || tl.Line != s.Truncated().Line {
					t.Errorf("Read %v, Scan %v; want both to report the same cut line", rerr, serr)
				}
			default:
				if rerr != nil || serr != nil {
					t.Errorf("Read %v, Scan %v; want both to take the file", rerr, serr)
				}
			}
		})
	}
}

// TestScanReports: what Scan yields and how it ends: a repeat is
// yielded once more with Repeat set, a cut last line ends it with a
// *TruncatedLine, a file before 0.5 is left to Read, and the sequence
// ranges once.
func TestScanReports(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sessions", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	repeated := strings.Join(append(lines, lines[1]), "\n")
	_, seq, err := Scan(strings.NewReader(repeated))
	if err != nil {
		t.Fatal(err)
	}
	repeats := 0
	for e, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		if e.Repeat {
			repeats++
		}
		if _, err := e.Decode(); err != nil {
			t.Errorf("Decode of %s: %v", e.ID, err)
		}
	}
	if repeats != 1 {
		t.Errorf("%d repeats, want 1", repeats)
	}
	n := 0
	for _, err := range seq {
		if err == nil {
			t.Fatal("a second range yields an entry")
		}
		n++
	}
	if n != 1 {
		t.Errorf("a second range yields %d errors, want 1", n)
	}
	cut := strings.Join(lines, "\n") + "\n" + lines[1][:len(lines[1])/2]
	_, seq, _ = Scan(strings.NewReader(cut))
	var last error
	for _, err := range seq {
		last = err
	}
	var tl *TruncatedLine
	if !errors.As(last, &tl) {
		t.Errorf("a cut last line ends with %v", last)
	}
	old := `{"type":"session","format":"agentsession/0.4","id":"x","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}` + "\n"
	if _, _, err := Scan(strings.NewReader(old)); !errors.Is(err, ErrScanMigrated) {
		t.Errorf("a 0.4 file: %v", err)
	}
}

// FuzzScanRead holds Scan to Read's verdict on arbitrary changes to a
// fixture.
func FuzzScanRead(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "sessions", "*.jsonl"))
	for _, p := range files {
		if data, err := os.ReadFile(p); err == nil {
			f.Add(data)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		agree(t, "fuzzed", data)
	})
}
