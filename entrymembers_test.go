package agentsession

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// TestEveryEntryMemberSurvivesARoundTrip is the guard the pinned member
// needed and did not have. Several entry types decode through a
// hand-written member list rather than through the struct, and
// jsonx.Keys derives the known-member set by reflection over the
// struct: the moment a field is added it stops being captured into
// EntryBase.Unknown, so a decoder that does not mention it drops it in
// silence. Nothing else in the suite sees that — TestRoundTripFixtures
// only covers members a fixture happens to carry.
//
// So: populate every JSON-tagged field on every entry type, write it,
// read it back, write it again, and require the member sets to match.
// It needs no discipline from whoever adds the next member.
func TestEveryEntryMemberSurvivesARoundTrip(t *testing.T) {
	for _, e := range []Entry{
		&ItemEntry{}, &ResponseEntry{}, &ConfigEntry{}, &CompactionEntry{},
		&BranchSummaryEntry{}, &LabelEntry{}, &InfoEntry{}, &EnvEntry{},
		&OutcomeEntry{}, &LinkEntry{}, &CustomEntry{},
		&RunEntry{}, &DispatchEntry{}, &DecisionEntry{}, &QueuedEntry{},
	} {
		typ := reflect.TypeOf(e).Elem()
		t.Run(typ.Name(), func(t *testing.T) {
			for _, variant := range entryVariants(e) {
				v := reflect.New(typ)
				fillStruct(t, v.Elem())
				entry := v.Interface().(Entry)
				base := entry.Base()
				base.ID = "e1"
				base.Parent = "p1"
				base.Timestamp = fixedTime
				base.Unknown = nil
				variant.set(entry)

				// The wire form is built from the struct rather than
				// taken from MarshalEntry, so that it carries every
				// declared member even when the encoder would leave
				// one out. That is what makes this see an encoder
				// that drops a member as well as a decoder that does.
				data := wireForm(t, entry)
				back, err := UnmarshalEntry(data)
				if err != nil {
					t.Fatalf("%s: unmarshal %s: %v", variant.name, data, err)
				}
				again, err := MarshalEntry(back)
				if err != nil {
					t.Fatalf("%s: re-marshal: %v", variant.name, err)
				}
				wrote, read := members(t, data), members(t, again)
				for key := range wrote {
					if _, ok := read[key]; !ok {
						t.Errorf("%s: member %q was dropped on the round trip.\nwrote:     %s\nread back: %s",
							variant.name, key, data, again)
					}
				}
				if u := back.Base().Unknown; len(u) > 0 {
					t.Errorf("%s: declared members parked in Unknown: %v", variant.name, u)
				}
			}
		})
	}
}

// variant is a shape an entry type has to be tested in, for the types
// whose encoding depends on a discriminator.
type variant struct {
	name string
	set  func(Entry)
}

func entryVariants(e Entry) []variant {
	if _, ok := e.(*RunEntry); ok {
		// A run's encoder writes one of two member lists by phase, so
		// both have to be exercised.
		return []variant{
			{"start", func(e Entry) { e.(*RunEntry).Phase = RunStart }},
			{"end", func(e Entry) { e.(*RunEntry).Phase = RunEnd }},
		}
	}
	return []variant{{"default", func(Entry) {}}}
}

// wireForm renders entry as a JSON object carrying the envelope and
// every member its struct declares, marshalling each field on its own
// so the type's own MarshalJSON cannot decide the member set.
func wireForm(t *testing.T, entry Entry) []byte {
	t.Helper()
	obj := map[string]json.RawMessage{
		"type":   mustJSON(t, entry.EntryType()),
		"id":     mustJSON(t, entry.Base().ID),
		"parent": mustJSON(t, entry.Base().Parent),
		"ts":     mustJSON(t, entry.Base().Timestamp),
	}
	v := reflect.ValueOf(entry).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		obj[name] = mustJSON(t, v.Field(i).Interface())
	}
	return mustJSON(t, obj)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return data
}

func members(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode members of %s: %v", data, err)
	}
	return m
}

// fillStruct sets every exported field to a non-zero value, so that
// every member the type declares reaches the wire.
func fillStruct(t *testing.T, v reflect.Value) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !v.Field(i).CanSet() || f.Tag.Get("json") == "-" {
			continue
		}
		if f.Anonymous && f.Type == reflect.TypeOf(EntryBase{}) {
			continue // the envelope is set by the caller
		}
		fillValue(t, v.Field(i), f.Name)
	}
}

var (
	itemType = reflect.TypeOf((*openresponses.Item)(nil)).Elem()
	toolType = reflect.TypeOf((*openresponses.Tool)(nil)).Elem()
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage(nil))
)

func fillValue(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	switch {
	case v.Type() == itemType:
		v.Set(reflect.ValueOf(openresponses.UserMessage(&openresponses.InputText{Text: name})))
		return
	case v.Type() == toolType:
		v.Set(reflect.ValueOf(openresponses.NewFunctionTool(name, "v1", nil)))
		return
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(fixedTime))
		return
	case v.Type() == rawType:
		v.Set(reflect.ValueOf(json.RawMessage(`{"k":1}`)))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(fillString(name))
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Float64:
		v.SetFloat(0.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillValue(t, p.Elem(), name)
		v.Set(p)
	case reflect.Slice:
		el := reflect.New(v.Type().Elem())
		fillValue(t, el.Elem(), name)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), el.Elem()))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key())
		fillValue(t, key.Elem(), name)
		val := reflect.New(v.Type().Elem())
		fillValue(t, val.Elem(), name)
		m.SetMapIndex(key.Elem(), val.Elem())
		v.Set(m)
	case reflect.Struct:
		fillStruct(t, v)
	case reflect.Interface:
		t.Fatalf("fillValue: no filler for interface field %s (%s)", name, v.Type())
	default:
		t.Fatalf("fillValue: no filler for field %s of kind %s", name, v.Kind())
	}
}

// fillString returns a value the entry validators accept, since an
// entry that will not marshal cannot be round-tripped.
func fillString(name string) string {
	switch name {
	case "Phase":
		return RunStart // overridden per variant
	case "Verdict":
		return VerdictReject
	case "Mode":
		return ModeSteer
	case "Status":
		return string(openresponses.ResponseStatusCompleted)
	}
	return name
}
