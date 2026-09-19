package export

import (
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
		return rewriteStrings(t, func(s string) string {
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
		return rewriteStrings(t, func(s string) string {
			s = strings.ReplaceAll(s, home+"/", "~/")
			s = strings.ReplaceAll(s, home+"\\", "~\\")
			return strings.ReplaceAll(s, home, "~")
		})
	})
}

// Environment removes environment snapshots from the document: the
// root and step extra.environment members and the working directory,
// which name machines, paths and file hashes that are rarely meant to
// travel.
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

// rewriteStrings applies f to every string in the document, extras
// included, by rewriting its JSON form. Numbers, booleans and member
// names are untouched.
func rewriteStrings(t *atif.Trajectory, f func(string) string) error {
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
