package export

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentsession/atif"
)

// Redactor rewrites an ATIF document in place. Redaction runs at
// export, never at write, and covers the extras as well as the steps.
type Redactor interface {
	Redact(*atif.Trajectory) error
}

// RedactorFunc adapts a function to Redactor.
type RedactorFunc func(*atif.Trajectory) error

// Redact implements Redactor.
func (f RedactorFunc) Redact(t *atif.Trajectory) error { return f(t) }

// Redact applies redactors to the document in order.
func Redact(doc *atif.Trajectory, redactors ...Redactor) error {
	for _, r := range redactors {
		if err := r.Redact(doc); err != nil {
			return err
		}
	}
	return nil
}

// Replacement is what redacted text becomes.
const Replacement = "[REDACTED]"

// NoPassthrough removes the raw Open Responses items and response
// bodies the exporter carries under every step's and observation
// result's extra.openresponses, which roughly halve a document and say
// nothing the declared fields do not. Use it for a judge's input or a
// published document; keep the full document in the archive, because
// the passthrough is what makes [Items] lossless, and without it Items
// returns [ErrNoRawItems], and the entries' envelopes and bodies under
// [ExtraEntries], without which a converter cannot rebuild the session to
// its ids. The root extra's payload profile name stays.
func NoPassthrough() Redactor {
	return RedactorFunc(func(t *atif.Trajectory) error {
		stripPassthrough(t)
		return nil
	})
}

func stripPassthrough(t *atif.Trajectory) {
	// The entries' envelopes and bodies rebuild the session with the raw
	// items, and without them rebuild nothing.
	if as, ok := t.Extra[ExtraAgentSession].(map[string]any); ok {
		delete(as, ExtraEntries)
	}
	for i := range t.Steps {
		step := &t.Steps[i]
		delete(step.Extra, ExtraOpenResponses)
		if step.Observation != nil {
			for j := range step.Observation.Results {
				delete(step.Observation.Results[j].Extra, ExtraOpenResponses)
			}
		}
	}
	for _, sub := range t.SubagentTrajectories {
		if sub != nil {
			stripPassthrough(sub)
		}
	}
}

// Secrets replaces every occurrence of the given values, in every
// string of the document, with [Replacement]. Empty values are
// ignored. Longer values are replaced first so a value that contains
// another is not left half redacted.
func Secrets(values ...string) Redactor {
	var secrets []string
	for _, v := range values {
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	sortByLengthDesc(secrets)
	return RedactorFunc(func(t *atif.Trajectory) error {
		if len(secrets) == 0 {
			return nil
		}
		return RewriteStrings(t, func(s string) string {
			for _, secret := range secrets {
				s = strings.ReplaceAll(s, secret, Replacement)
			}
			return s
		})
	})
}

// HomePaths replaces absolute paths under home with "~"-relative ones
// in every string of the document, and the home directory itself with
// "~". With an empty home it does nothing.
func HomePaths(home string) Redactor {
	home = filepath.Clean(home)
	return RedactorFunc(func(t *atif.Trajectory) error {
		if home == "" || home == "." || home == "/" {
			return nil
		}
		return RewriteStrings(t, func(s string) string {
			s = strings.ReplaceAll(s, home+"/", "~/")
			s = strings.ReplaceAll(s, home+"\\", "~\\")
			return strings.ReplaceAll(s, home, "~")
		})
	})
}

// Environment removes environment snapshots from the document: the
// root and step extra.environment members, the working directory and
// each env entry's body under [ExtraEntries], which name machines, paths
// and file hashes that are rarely meant to travel. An env entry so
// stripped no longer rebuilds to its id.
func Environment() Redactor {
	return RedactorFunc(func(t *atif.Trajectory) error {
		scrub := func(extra map[string]any) {
			delete(extra, ExtraEnvironment)
			if as, ok := extra[ExtraAgentSession].(map[string]any); ok {
				delete(as, "cwd")
			}
		}
		if t.Extra != nil {
			scrub(t.Extra)
			// An env entry's body, which names the same, beside its
			// envelope under ExtraEntries.
			if as, ok := t.Extra[ExtraAgentSession].(map[string]any); ok {
				entries, _ := as[ExtraEntries].(map[string]any)
				for _, v := range entries {
					rec, _ := v.(map[string]any)
					if typ, _ := json.Marshal(rec["type"]); string(typ) == `"env"` {
						delete(rec, "body")
						delete(rec, "normalised")
					}
				}
			}
		}
		for i := range t.Steps {
			if t.Steps[i].Extra != nil {
				scrub(t.Steps[i].Extra)
			}
		}
		for _, sub := range t.SubagentTrajectories {
			if sub != nil {
				if err := Environment().Redact(sub); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RewriteStrings applies f to every string in the document, extras
// included, by rewriting its JSON form, as [Secrets] and [HomePaths] do,
// for a custom [RedactorFunc]. Numbers, booleans and member names are
// untouched. It first drops each
// normalised record f would change, which holds the member it describes
// as source text and base64 bytes a rewrite of strings does not reach;
// that entry then does not rebuild to its id.
func RewriteStrings(t *atif.Trajectory, f func(string) string) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode for redaction: %w", err)
	}
	var tree any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return fmt.Errorf("decode for redaction: %w", err)
	}
	dropNormalised(tree, f)
	tree = walk(tree, f)
	out, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("re-encode after redaction: %w", err)
	}
	var back atif.Trajectory
	if err := json.Unmarshal(out, &back); err != nil {
		return fmt.Errorf("decode after redaction: %w", err)
	}
	*t = back
	return nil
}

// dropNormalised removes, anywhere in a document, a normalised record
// that f would change: an entry's under [ExtraEntries], a response's in
// its body. Each element holds the member it describes as source text,
// was, and as original bytes, raw, in base64, which a rewrite of the
// document's strings does not reach, so a redactor that would change
// one drops the record (RFC 0001, Redaction); the entry then does not
// rebuild to its id, which a converter lists.
func dropNormalised(v any, f func(string) string) {
	switch x := v.(type) {
	case map[string]any:
		if list, ok := x["normalised"].([]any); ok && normalisedTouched(list, f) {
			delete(x, "normalised")
		}
		for _, c := range x {
			dropNormalised(c, f)
		}
	case []any:
		for _, c := range x {
			dropNormalised(c, f)
		}
	}
}

// normalisedTouched reports whether f changes what a normalised record
// holds: was, as written and as the JSON it is the source text of, or
// raw's bytes.
func normalisedTouched(list []any, f func(string) string) bool {
	changes := func(s string) bool { return f(s) != s }
	for _, el := range list {
		m, _ := el.(map[string]any)
		if was, ok := m["was"].(string); ok {
			if changes(was) {
				return true
			}
			var decoded any
			if json.Unmarshal([]byte(was), &decoded) == nil {
				var hit bool
				walk(decoded, func(s string) string {
					hit = hit || changes(s)
					return s
				})
				if hit {
					return true
				}
			}
		}
		if raw, ok := m["raw"].(string); ok {
			b, err := base64.StdEncoding.DecodeString(raw)
			if err != nil || changes(string(b)) {
				return true
			}
		}
	}
	return false
}

func walk(v any, f func(string) string) any {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for i := range x {
			x[i] = walk(x[i], f)
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = walk(x[k], f)
		}
		return x
	}
	return v
}

func sortByLengthDesc(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
