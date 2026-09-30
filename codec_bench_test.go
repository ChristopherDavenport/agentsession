package agentsession

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// largeSession builds a session of calls rounds, each a user message,
// a function call, its output of outputSize bytes and the response.
func largeSession(tb testing.TB, calls, outputSize int) []byte {
	tb.Helper()
	s := New(Header{CWD: "/p"})
	must := func(_ string, err error) {
		if err != nil {
			tb.Fatal(err)
		}
	}
	must(s.Append(&ConfigEntry{Model: "m", Instructions: ptr("Be brief.")}))
	output := strings.Repeat("tool output line\n", outputSize/len("tool output line\n")+1)[:outputSize]
	for i := 0; i < calls; i++ {
		// A call ID names one call in the session, so each round has its own.
		callID, respID := fmt.Sprintf("call_%d", i), fmt.Sprintf("resp_%d", i)
		must(s.Append(&ItemEntry{Item: &openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputText{Text: "run it"}}}}))
		must(s.Append(&ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "run", Arguments: `{"cmd":"make"}`}, ResponseID: respID}))
		must(s.Append(&ResponseEntry{ResponseID: respID, Model: "m", Status: openresponses.ResponseStatusCompleted, RequestHash: "sha256:0"}))
		must(s.Append(&ItemEntry{Item: &openresponses.FunctionCallOutput{CallID: callID, Output: openresponses.FunctionCallOutputData{Text: output}}}))
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func BenchmarkRead(b *testing.B) {
	for _, tc := range []struct {
		name          string
		calls, output int
	}{
		{"small-lines", 2000, 200},
		{"large-lines", 100, 100 << 10},
	} {
		data := largeSession(b, tc.calls, tc.output)
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Read(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
