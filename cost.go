package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/openresponses"
)

// ModelCall is an entry recording model calls the path paid for: a
// response, as many as its attempts; a compaction or a branch summary,
// one; or a custom entry whose data carries a usage in the payload's
// shape, as many as the attempts beside it, one when absent (RFC 0001,
// Projections/ATIF, total_steps).
type ModelCall struct {
	Entry Entry
	// Model is the model the calls are priced at: the one the entry
	// names, or the model in force on the path when it names none.
	Model string
	// Usage is what the entry records, nil when it records none.
	Usage *openresponses.Usage
	// Calls is how many model calls the entry records, at least one.
	Calls int
}

// ModelCalls returns the model calls a root-first path records, in path
// order, each priced at the model that served it: the one it names, or
// the one the config entries, or a compaction's checkpoint, have in
// force before it.
func ModelCalls(path []Entry) []ModelCall {
	var out []ModelCall
	model := ""
	for _, e := range path {
		switch v := e.(type) {
		case *ConfigEntry:
			if v.Replace {
				model = ""
			}
			if v.Model != "" {
				model = v.Model
			}
			continue
		case *CompactionEntry:
			model = v.Config.Model
		}
		if c, ok := modelCallOf(e, model); ok {
			out = append(out, c)
		}
	}
	return out
}

// modelCallOf returns the model calls e records, priced at inForce when
// it names no model of its own, and whether it records any.
func modelCallOf(e Entry, inForce string) (ModelCall, bool) {
	c := ModelCall{Entry: e, Model: inForce, Calls: 1}
	switch v := e.(type) {
	case *ResponseEntry:
		c.Usage, c.Calls = v.Usage, v.Calls()
		if v.Model != "" {
			c.Model = v.Model
		}
	case *CompactionEntry:
		c.Usage, c.Model = v.Usage, v.Config.Model
	case *BranchSummaryEntry:
		c.Usage = v.Usage
	case *CustomEntry:
		var paid struct {
			Usage *openresponses.Usage `json:"usage"`
			Model string               `json:"model"`
		}
		if json.Unmarshal(v.Data, &paid) != nil || paid.Usage == nil {
			return ModelCall{}, false
		}
		c.Usage, c.Calls = paid.Usage, v.Calls()
		if paid.Model != "" {
			c.Model = paid.Model
		}
	default:
		return ModelCall{}, false
	}
	return c, true
}

// Delegated is a session whose work a path delegated, by a subsession
// link on the path (RFC 0001, link, Cost across sessions, since 0.17).
type Delegated struct {
	// Link is the first subsession link on the path naming the session.
	Link *LinkEntry
	// Session is the child's ID.
	Session string
	// Leaf is the entry of the child whose path is counted: the one the
	// entry carrying the call's output names in its parents, or, when
	// none does, the leaf a reader resuming the child takes, and then
	// LeafNamed is false, which a reader reporting cost says.
	Leaf      string
	LeafNamed bool
	// Calls are the model calls on the child's path to Leaf, each priced
	// at the model that served it in the child.
	Calls []ModelCall
	// Children are the child's own delegated sessions, counted by the
	// same rule.
	Children []Delegated
}

// Unread is a child a reader reporting cost could not read, and left
// out rather than guess.
type Unread struct {
	Link    *LinkEntry
	Session string
	Err     error
}

// ErrChildNotFound is the error of an [Unread] child the resolver did
// not find, or that holds no entry by the leaf the parent named.
var ErrChildNotFound = errors.New("agentsession: delegated session not found")

// ErrChildElsewhere is the error of an [Unread] callee in another system
// whose session the reader does not hold.
var ErrChildElsewhere = errors.New("agentsession: delegated session held in another system")

// DelegatedWork returns the sessions a root-first path delegated work
// to, as a reader that reports what the path cost, including that work,
// counts them (RFC 0001, link, since 0.17): each session a subsession
// link on the path names, cancel notices excepted, counted once however
// many links name it, at the leaf the call's output names in parents or
// else at the child's resume leaf, with its own children by the same
// rule. A child resolve cannot read, or that holds no entry by the leaf
// named, is reported in unread and left out ([ErrChildNotFound]). A
// subsession link carrying a url names a callee in another system: it is
// counted when resolve holds its session, and otherwise reported in
// unread as one held elsewhere ([ErrChildElsewhere]). Peer callees,
// judges and forks are no delegated work and are not counted.
// resolve returns nil and no error for a session it does not hold.
func DelegatedWork(path []Entry, resolve func(id string) (*Session, error)) (children []Delegated, unread []Unread) {
	seen := map[string]bool{}
	return delegated(path, resolve, seen)
}

func delegated(path []Entry, resolve func(id string) (*Session, error), seen map[string]bool) (children []Delegated, unread []Unread) {
	outputs := map[string]*ItemEntry{}
	for _, c := range Calls(path) {
		if c.Output != nil {
			outputs[c.ID()] = c.Output
		}
	}
	for _, e := range path {
		l, ok := e.(*LinkEntry)
		if !ok || l.Rel != RelSubsession || l.Cancel != "" || l.Session == "" || seen[l.Session] {
			continue
		}
		seen[l.Session] = true
		child, err := resolve(l.Session)
		if child == nil || err != nil {
			switch {
			case l.URL != "" && err == nil:
				// A callee in another system whose session the reader
				// does not hold: delegated work it cannot count.
				err = fmt.Errorf("%w: %s at %s", ErrChildElsewhere, l.Session, l.URL)
			case err == nil:
				err = fmt.Errorf("%w: %s", ErrChildNotFound, l.Session)
			}
			unread = append(unread, Unread{Link: l, Session: l.Session, Err: err})
			continue
		}
		d := Delegated{Link: l, Session: l.Session, Leaf: child.Leaf()}
		if out := outputs[l.CallID]; out != nil {
			for _, r := range out.Parents {
				if r.Session == l.Session {
					d.Leaf, d.LeafNamed = r.Entry, true
					break
				}
			}
		}
		childPath := child.Path(d.Leaf)
		if childPath == nil {
			unread = append(unread, Unread{Link: l, Session: l.Session, Err: fmt.Errorf("%w: %s holds no entry %s", ErrChildNotFound, l.Session, d.Leaf)})
			continue
		}
		d.Calls = ModelCalls(childPath)
		var more []Unread
		d.Children, more = delegated(childPath, resolve, seen)
		unread = append(unread, more...)
		children = append(children, d)
	}
	return children, unread
}
