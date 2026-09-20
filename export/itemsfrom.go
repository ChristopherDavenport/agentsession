package export

import (
	"encoding/json"
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
func ItemsFrom(doc *atif.Trajectory) (openresponses.Items, error) {
	var items openresponses.Items
	for _, s := range doc.Steps {
		switch s.Source {
		case atif.SourceUser:
			items = append(items, &openresponses.Message{Role: openresponses.RoleUser, Content: inputParts(s.Message)})
		case atif.SourceSystem:
			if !s.Message.IsZero() || s.Observation == nil {
				items = append(items, &openresponses.Message{Role: openresponses.RoleSystem, Content: inputParts(s.Message)})
			}
			items = append(items, outputsOf(s.Observation)...)
		case atif.SourceAgent:
			if s.ReasoningContent != "" {
				items = append(items, &openresponses.ReasoningItem{Summary: openresponses.Contents{&openresponses.SummaryText{Text: s.ReasoningContent}}})
			}
			if text := s.Message.String(); text != "" {
				items = append(items, openresponses.AssistantText(text))
			}
			for _, tc := range s.ToolCalls {
				items = append(items, &openresponses.FunctionCall{
					CallID:    tc.ToolCallID,
					Name:      tc.FunctionName,
					Arguments: argumentsText(tc.Arguments),
				})
			}
			items = append(items, outputsOf(s.Observation)...)
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
