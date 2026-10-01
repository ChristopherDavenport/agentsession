package export

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// ItemsFrom rebuilds a conversation from an ATIF document's declared
// fields alone, so a document from any producer loads, not only one
// this package wrote. It is lossy where the declared fields are: use
// [Items] on a document that carries the raw items. Per step, a user
// step becomes a user message; a system step a system message, with
// any observation result that names a call becoming that call's
// output; an agent step a reasoning item from reasoning_content, an
// assistant message from message when it has text, one function call
// per tool call, and one function call output per observation result
// that names a call. Image parts keep their path as the image URL.
//
// Lost: item IDs and statuses, message phases, encrypted reasoning
// and the summary-versus-content distinction, the original bytes of
// tool call arguments (re-encoded from the document's object, so key
// order may change and a call whose arguments were not an object
// comes back from its "_arguments" member), non-text tool outputs
// (flattened to text), visibility flags, extension items (only their
// text survives), and audio parts (a text placeholder). A copied
// context step is a system message like any other; the document's
// is_copied_context flag does not survive.
//
// A call ID names one call in a session, and ATIF lets a document
// repeat a tool call ID, across steps as a producer that numbers calls
// per turn does, or within one. A tool call whose ID an earlier call
// used, or that has none, gets one of its own: the native ID with every
// character outside letters, digits, "_" and "-" made "_", cut so the
// whole is at most 64 characters, followed by "_" and a number ("call"
// when it has none), which a provider that limits call IDs to that
// alphabet and length still takes, as agentturn's renamer does. The
// observation results that name a native ID take the step's calls with
// it in order, the last taking any results left over; ATIF puts a
// result in the step of its call, so the renames hold until the next
// agent step. The native ID survives only as the new one's prefix.
func ItemsFrom(doc *atif.Trajectory) (openresponses.Items, error) {
	var items openresponses.Items
	seen := map[string]bool{}
	// ids maps a native call ID to the IDs the current agent step's
	// calls with it were given, in order, for its results to take.
	ids := map[string][]string{}
	callID := func(native string) string {
		base := callIDBase(native)
		id := native
		for n := 2; id == "" || seen[id]; n++ {
			suffix := fmt.Sprintf("_%d", n)
			id = base[:min(len(base), maxCallID-len(suffix))] + suffix
		}
		seen[id] = true
		ids[native] = append(ids[native], id)
		return id
	}
	outputs := func(o *atif.Observation) openresponses.Items {
		out := outputsOf(o)
		for _, it := range out {
			fo, ok := it.(*openresponses.FunctionCallOutput)
			if !ok {
				continue
			}
			native := fo.CallID
			if q := ids[native]; len(q) > 0 {
				fo.CallID = q[0]
				if len(q) > 1 {
					ids[native] = q[1:]
				}
			}
		}
		return out
	}
	for _, s := range doc.Steps {
		switch s.Source {
		case atif.SourceUser:
			items = append(items, &openresponses.Message{Role: openresponses.RoleUser, Content: inputParts(s.Message)})
		case atif.SourceSystem:
			if !s.Message.IsZero() || s.Observation == nil {
				items = append(items, &openresponses.Message{Role: openresponses.RoleSystem, Content: inputParts(s.Message)})
			}
			items = append(items, outputs(s.Observation)...)
		case atif.SourceAgent:
			clear(ids)
			if s.ReasoningContent != "" {
				items = append(items, &openresponses.ReasoningItem{Summary: openresponses.Contents{&openresponses.SummaryText{Text: s.ReasoningContent}}})
			}
			if text := s.Message.String(); text != "" {
				items = append(items, openresponses.AssistantText(text))
			}
			for _, tc := range s.ToolCalls {
				items = append(items, &openresponses.FunctionCall{
					CallID:    callID(tc.ToolCallID),
					Name:      tc.FunctionName,
					Arguments: argumentsText(tc.Arguments),
				})
			}
			items = append(items, outputs(s.Observation)...)
		}
	}
	return items, nil
}

// inputParts maps content to input parts: text to input_text, an
// image to input_image by its path, audio to a text placeholder.
func inputParts(c atif.Content) openresponses.Contents {
	if c.Parts == nil {
		return openresponses.Contents{&openresponses.InputText{Text: c.Text}}
	}
	var out openresponses.Contents
	for _, p := range c.Parts {
		switch p.Type {
		case atif.PartImage:
			if p.Source != nil {
				out = append(out, &openresponses.InputImage{ImageURL: p.Source.Path})
			}
		case atif.PartAudio:
			path := ""
			if p.Source != nil {
				path = p.Source.Path
			}
			out = append(out, &openresponses.InputText{Text: "[audio: " + path + "]"})
		default:
			out = append(out, &openresponses.InputText{Text: p.Text})
		}
	}
	return out
}

// outputsOf returns a function call output for every observation
// result that names its call.
func outputsOf(o *atif.Observation) openresponses.Items {
	if o == nil {
		return nil
	}
	var out openresponses.Items
	for _, r := range o.Results {
		if r.SourceCallID == "" {
			continue
		}
		out = append(out, openresponses.NewFunctionCallOutput(r.SourceCallID, r.Content.String()))
	}
	return out
}

// argumentsText re-encodes a tool call's arguments as the JSON string
// a function call carries. The exporter stores arguments that were not
// an object under "_arguments"; those come back as they were.
func argumentsText(args map[string]any) string {
	if raw, ok := args["_arguments"]; ok && len(args) == 1 {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	if args == nil {
		return "{}"
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return strings.TrimSpace(string(data))
}

// maxCallID is the longest call ID ItemsFrom makes up, which OpenAI and
// Anthropic both take.
const maxCallID = 64

// callIDBase is a native call ID in the alphabet every provider takes
// for a call ID, letters, digits, '_' and '-', anything else made '_',
// or "call" when it is empty.
func callIDBase(native string) string {
	if native == "" {
		return "call"
	}
	b := []byte(native)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}
