package agentsession

import (
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestCallIDEmpty: a function call with no call ID is refused on
// append and reported by VerifyRecords in a file another writer made
// (#135).
func TestCallIDEmpty(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", Name: "t", Arguments: "{}"}}); !errors.Is(err, ErrCallIDEmpty) {
		t.Errorf("Append = %v, want ErrCallIDEmpty", err)
	}
	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"","name":"t","arguments":"{}"}`
	in, _ := hashedLines(t, Format, call)
	read, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, ErrCallIDEmpty) {
		t.Errorf("VerifyRecords = %v, want ErrCallIDEmpty", err)
	}
}

// TestSourceShape: a run start's source is the shape of its segment, a
// resume taking up a call pending when the run began and an input
// otherwise, and VerifyRecords reports one that is not (#147).
func TestSourceShape(t *testing.T) {
	held := "start user calls:a resp hold:a end:input_required "
	for _, tt := range []struct {
		name, script string
		want         error
	}{
		{"resume takes up the held call", held + "start:resume proceed:a dispatch:a out:a resp end:done", nil},
		{"resume after a message", held + "start:resume user proceed:a dispatch:a out:a resp end:done", nil},
		{"input takes up the held call", held + "start:input user proceed:a dispatch:a out:a resp end:done", ErrSourceMismatch},
		{"resume takes up nothing", held + "start:resume user resp end:done", ErrSourceMismatch},
		{"first run as a resume", "start:resume user resp end:done", ErrSourceMismatch},
		{"input with its own call", "start user calls:b resp dispatch:b out:b resp end:done", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Header{Records: AllRecords})
			for _, e := range seg(t, tt.script) {
				b := e.Base()
				b.ID, b.Parent = "", ""
				if r, ok := e.(*RunEntry); ok && r.IsEnd() {
					if end, err := s.EndRun(r.Reason, ""); err == nil {
						e = end
					}
				}
				if _, err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, tt.want) {
				t.Errorf("VerifyRecords = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestForkPromise: the rules resting on a header's records promise
// apply to what the session wrote, after its base; a fork promising
// dispatch of a session that promised nothing verifies, and its own
// call with no dispatch does not (#144).
func TestForkPromise(t *testing.T) {
	origin := New(Header{})
	for _, e := range seg(t, "user calls:a resp out:a resp") {
		b := e.Base()
		b.ID, b.Parent = "", ""
		if _, err := origin.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := origin.VerifyRecords(origin.Leaf()); err != nil {
		t.Fatalf("the origin: %v", err)
	}
	fork, err := Fork(origin, origin.Leaf(), Header{Records: []string{TypeDispatch}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fork.VerifyRecords(fork.Leaf()); err != nil {
		t.Errorf("a fork whose prefix promised nothing: %v", err)
	}
	target, err := fork.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(NewDispatch("b", target)); err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(NewItemEntry(openresponses.NewFunctionCallOutput("b", "ran"))); err != nil {
		t.Fatal(err)
	}
	if err := fork.VerifyRecords(fork.Leaf()); err != nil {
		t.Errorf("the fork's own dispatched call: %v", err)
	}
	// Read back as the file it projects to, the prefix carried in it.
	var buf strings.Builder
	if err := Write(&buf, fork); err != nil {
		t.Fatal(err)
	}
	read, err := Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); err != nil {
		t.Errorf("the fork read back: %v", err)
	}
	// The fork's own call with an output and no dispatch is still one.
	bare, err := Fork(origin, origin.Leaf(), Header{Records: []string{TypeDispatch}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "t", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Append(NewItemEntry(openresponses.NewFunctionCallOutput("c", "ran"))); err != nil {
		t.Fatal(err)
	}
	if err := bare.VerifyRecords(bare.Leaf()); !errors.Is(err, ErrRecordMissing) {
		t.Errorf("the fork's own undispatched call: %v, want ErrRecordMissing", err)
	}
}
