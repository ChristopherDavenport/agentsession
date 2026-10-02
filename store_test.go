package agentsession_test

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/storetest"
)

func TestMemoryStore(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return agentsession.NewMemoryStore() },
	})
}

// TestListFilterMatches: the header-level filters decide on the header
// alone, and the zero filter keeps every header. Harness is the
// harness's name, matched exactly and by no header naming none; Extra
// is matched member by member in canonical form, so the spelling of a
// value does not matter and members the header has beyond the filter's
// do not count; TopLevel keeps a header with no spawned_by (#83).
func TestListFilterMatches(t *testing.T) {
	routine := agentsession.Header{
		Harness:   &agentsession.Harness{Name: "nightly", Version: "2"},
		SpawnedBy: "",
		Extra:     map[string]json.RawMessage{"acme:platform": json.RawMessage(`"telegram"`), "acme:user": json.RawMessage(`{"name": "u1", "id": 7}`)},
	}
	spawned := routine
	spawned.SpawnedBy = "call_1"
	bare := agentsession.Header{}
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	tests := []struct {
		name string
		f    agentsession.ListFilter
		h    agentsession.Header
		want bool
	}{
		{"zero filter, bare header", agentsession.ListFilter{}, bare, true},
		{"zero filter", agentsession.ListFilter{}, routine, true},
		{"harness", agentsession.ListFilter{Harness: "nightly"}, routine, true},
		{"harness differs", agentsession.ListFilter{Harness: "cli"}, routine, false},
		{"harness, header names none", agentsession.ListFilter{Harness: "nightly"}, bare, false},
		{"top level", agentsession.ListFilter{TopLevel: true}, routine, true},
		{"top level, spawned", agentsession.ListFilter{TopLevel: true}, spawned, false},
		{"top level, bare", agentsession.ListFilter{TopLevel: true}, bare, true},
		{"extra", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:platform": raw(`"telegram"`)}}, routine, true},
		{"extra spelled apart", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:user": raw(`{"id":7,"name":"u1"}`)}}, routine, true},
		{"extra differs", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:user": raw(`{"id":8,"name":"u1"}`)}}, routine, false},
		{"extra the header lacks", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:zone": raw(`1`)}}, routine, false},
		{"extra, bare header", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:platform": raw(`"telegram"`)}}, bare, false},
		{"extra not JSON", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:platform": raw(`telegram`)}}, routine, false},
		{"empty extra", agentsession.ListFilter{Extra: map[string]json.RawMessage{}}, bare, true},
		{"all three", agentsession.ListFilter{Harness: "nightly", TopLevel: true, Extra: map[string]json.RawMessage{"acme:platform": raw(`"telegram"`)}}, routine, true},
		{"all three, spawned", agentsession.ListFilter{Harness: "nightly", TopLevel: true, Extra: map[string]json.RawMessage{"acme:platform": raw(`"telegram"`)}}, spawned, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Matches(tt.h); got != tt.want {
				t.Errorf("Matches = %v, want %v", got, tt.want)
			}
			if got := tt.f.Keep(agentsession.Summary{Header: tt.h}); got != tt.want {
				t.Errorf("Keep = %v, want %v", got, tt.want)
			}
		})
	}
}
