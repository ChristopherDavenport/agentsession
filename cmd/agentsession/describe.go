package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// textWidth bounds the free text shown for an entry or item.
const textWidth = 72

// describeEntry renders one entry on one line.
func describeEntry(e agentsession.Entry) string {
	switch v := e.(type) {
	case *agentsession.ItemEntry:
		s := describeItem(v.Item)
		if v.ResponseID != "" {
			s += " (" + v.ResponseID + ")"
		}
		if v.Visible != nil && !*v.Visible {
			s = "hidden " + s
		}
		return s
	case *agentsession.ResponseEntry:
		parts := []string{v.ResponseID, orDash(v.Model), string(v.Status)}
		if v.Usage != nil {
			parts = append(parts, fmt.Sprintf("%d+%d tokens", v.Usage.InputTokens, v.Usage.OutputTokens))
		}
		if v.RequestHash != "" {
			parts = append(parts, "hash "+shorten(v.RequestHash, 19))
		}
		if v.Error != nil {
			parts = append(parts, "error "+describeText(v.Error.Message))
		}
		return strings.Join(parts, " ")
	case *agentsession.ConfigEntry:
		var parts []string
		if v.Replace {
			parts = append(parts, "replace")
		}
		if v.Model != "" {
			parts = append(parts, "model "+v.Model)
		}
		if v.Instructions != nil {
			parts = append(parts, "instructions "+describeText(*v.Instructions))
		}
		if len(v.InstructionsParts) > 0 {
			parts = append(parts, "instructions "+describeParts(v.InstructionsParts))
		}
		if n := len(v.InstructionsOmitted); n > 0 {
			ids := make([]string, 0, n)
			for _, o := range v.InstructionsOmitted {
				ids = append(ids, o.ID+" ("+o.Reason+")")
			}
			parts = append(parts, fmt.Sprintf("omitted %s", strings.Join(ids, ", ")))
		}
		if v.Reasoning != nil {
			parts = append(parts, "reasoning")
		}
		if v.Text != nil {
			parts = append(parts, "text")
		}
		for _, t := range v.ToolsAdded {
			parts = append(parts, "+"+orDash(agentsession.ToolName(t)))
		}
		for _, n := range v.ToolsRemoved {
			parts = append(parts, "-"+n)
		}
		for _, k := range sortedKeys(v.Extra) {
			parts = append(parts, "extra."+k)
		}
		if len(parts) == 0 {
			return "no change"
		}
		return strings.Join(parts, ", ")
	case *agentsession.CompactionEntry:
		s := "first kept " + v.FirstKept + "; " + describeItem(v.Summary)
		if v.TokensBefore > 0 {
			s += fmt.Sprintf(" (%d tokens before)", v.TokensBefore)
		}
		return s
	case *agentsession.BranchSummaryEntry:
		return "from " + v.From + "; " + describeItem(v.Summary)
	case *agentsession.LabelEntry:
		if v.Label == nil {
			return v.Target + " cleared"
		}
		return v.Target + " = " + *v.Label
	case *agentsession.InfoEntry:
		if v.Name != "" {
			return "name " + describeText(v.Name)
		}
		return "info"
	case *agentsession.EnvEntry:
		var parts []string
		if v.CWD != "" {
			parts = append(parts, "cwd "+v.CWD)
		}
		if v.VCS != nil {
			s := v.VCS.System + " " + shorten(v.VCS.Revision, 12)
			if v.VCS.Dirty {
				s += " dirty"
			}
			parts = append(parts, s)
		}
		if v.Files != nil {
			parts = append(parts, fmt.Sprintf("%d read, %d written", len(v.Files.Read), len(v.Files.Written)))
		}
		if len(v.Tools) > 0 {
			parts = append(parts, fmt.Sprintf("%d tool(s)", len(v.Tools)))
		}
		if v.Workspace != nil {
			parts = append(parts, strings.TrimSpace(v.Workspace.Kind+" "+shorten(v.Workspace.Ref, 20)))
		}
		return strings.Join(parts, ", ")
	case *agentsession.OutcomeEntry:
		parts := []string{v.Kind}
		if v.Target != "" {
			parts = append(parts, "on "+v.Target)
		}
		if v.Score != nil {
			parts = append(parts, fmt.Sprintf("score %g", *v.Score))
		}
		if v.Pass != nil {
			if *v.Pass {
				parts = append(parts, "pass")
			} else {
				parts = append(parts, "fail")
			}
		}
		if v.Label != "" {
			parts = append(parts, v.Label)
		}
		return strings.Join(parts, " ")
	case *agentsession.LinkEntry:
		s := v.Rel + " " + v.Session
		if v.CallID != "" {
			s += " via " + v.CallID
		}
		return s
	case *agentsession.RunEntry:
		parts := []string{v.Phase, v.RunID}
		if v.IsStart() {
			parts = append(parts, v.Source)
		} else {
			parts = append(parts, v.Reason)
			if len(v.Pending) > 0 {
				parts = append(parts, fmt.Sprintf("pending %s", strings.Join(v.Pending, ",")))
			}
		}
		if v.Ref != "" {
			parts = append(parts, "ref "+describeText(v.Ref))
		}
		return strings.Join(parts, " ")
	case *agentsession.DispatchEntry:
		return v.CallID + " to tool"
	case *agentsession.DecisionEntry:
		parts := []string{v.Verdict, v.CallID}
		if v.By != "" {
			parts = append(parts, "by "+v.By)
		}
		if v.Reason != "" {
			parts = append(parts, describeText(v.Reason))
		}
		if len(v.Args) > 0 {
			parts = append(parts, "args rewritten")
		}
		return strings.Join(parts, " ")
	default:
		return "(unknown entry type)"
	}
}

// describeParts renders a config entry's instruction parts: the ID of
// every part in order, with the size of the ones that carry their
// text and "=" for the ones a delta names by hash alone.
func describeParts(parts []agentsession.InstructionPart) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Text == "" && p.Hash != "":
			out = append(out, p.ID+"=")
		default:
			out = append(out, fmt.Sprintf("%s(%dB)", p.ID, len(p.Text)))
		}
	}
	return "[" + strings.Join(out, " ") + "]"
}

// describeItem renders one Open Responses item on one line.
func describeItem(it openresponses.Item) string {
	switch v := it.(type) {
	case nil:
		return "-"
	case *openresponses.Message:
		text := v.Content.Text()
		if text == "" {
			return string(v.Role) + ": " + partTypes(v.Content)
		}
		return string(v.Role) + ": " + describeText(text)
	case *openresponses.FunctionCall:
		return "call " + v.Name + "(" + describeText(v.Arguments) + ")"
	case *openresponses.FunctionCallOutput:
		text := v.Output.Text
		if v.Output.Parts != nil {
			text = v.Output.Parts.Text()
			if text == "" {
				text = partTypes(v.Output.Parts)
			}
		}
		return "output " + v.CallID + ": " + describeText(text)
	case *openresponses.ReasoningItem:
		text := v.Summary.Text()
		if text == "" {
			text = v.Content.Text()
		}
		if text == "" && v.EncryptedContent != "" {
			return "reasoning (encrypted)"
		}
		return "reasoning: " + describeText(text)
	case *openresponses.Compaction:
		return "compaction (encrypted)"
	case *openresponses.ItemReference:
		return "reference " + v.ID
	default:
		return it.ItemType()
	}
}

// partTypes lists the content types of parts without text.
func partTypes(c openresponses.Contents) string {
	if len(c) == 0 {
		return "(empty)"
	}
	types := make([]string, len(c))
	for i, p := range c {
		types[i] = p.ContentType()
	}
	return "[" + strings.Join(types, ", ") + "]"
}

// describeText collapses whitespace and bounds the length, quoting
// the result so its edges are visible.
func describeText(s string) string {
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	s = strings.Join(fields, " ")
	if s == "" {
		return `""`
	}
	return fmt.Sprintf("%q", shorten(s, textWidth))
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
