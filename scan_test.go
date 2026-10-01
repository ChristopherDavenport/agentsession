package agentsession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// check, a core entry's members, so that case is not held here.
func agree(t *testing.T, name string, data []byte) {
	t.Helper()
	ids, serr := scanAll(data)
	s, rerr := Read(bytes.NewReader(data))
	if errors.Is(serr, ErrScanMigrated) {
		return
	}
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

// TestScanReports: what Scan yields and how it ends: a repeat is
// yielded once more with Repeat set, a cut last line ends it with a
// *TruncatedLine, a file before 0.5 is left to Read.
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
