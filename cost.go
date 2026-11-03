package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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
	// Link is the first subsession link naming the session.
	Link *LinkEntry
	// Session is the child's ID.
	Session string
	// Leaves are the entries of the child whose paths are counted, one
	// for each link naming it that names another: the one the entry
	// carrying the call's output names in its parents, or, when none
	// does, the leaf a reader resuming the child takes, and then Named is
	// false, which a reader reporting cost says.
	Leaves []DelegatedLeaf
	// Calls are the model calls on the union of the child's paths to its
	// Leaves, each entry once, each priced at the model that served it in
	// the child.
	Calls []ModelCall
	// Children are the child's own delegated sessions, counted by the
	// same rule.
	Children []Delegated
}

// DelegatedLeaf is an entry of a delegated session whose path a reader
// counts, and whether a parents member named it.
type DelegatedLeaf struct {
	Entry string
	Named bool
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
// many links name it, over the union of its paths to the leaf each link
// counts, each entry once: the leaf the call's output names in parents,
// or else the child's resume leaf. Its own children are counted by the
// same rule. A child resolve cannot read, or that holds no entry by the leaf
// named, is reported in unread and left out ([ErrChildNotFound]). A
// subsession link carrying a url names a callee in another system: it is
// counted when resolve holds its session, and otherwise reported in
// unread as one held elsewhere ([ErrChildElsewhere]). Peer callees,
// judges and forks are no delegated work and are not counted.
// resolve returns nil and no error for a session it does not hold.
func DelegatedWork(path []Entry, resolve func(id string) (*Session, error)) (children []Delegated, unread []Unread) {
	type node struct {
		d       Delegated
		session *Session
		parent  string // the session that named it first; "" for path's
		scanned int    // how many of its leaves its links were read on
	}
	nodes := map[string]*node{}
	var order []string
	failed := map[string]bool{}
	// scan registers the subsession links on paths, the paths of the
	// session from: each child it names, and the leaf each link counts.
	scan := func(paths [][]Entry, from string) {
		outputs := map[string]*ItemEntry{}
		for _, p := range paths {
			for _, c := range Calls(p) {
				if c.Output != nil {
					outputs[c.ID()] = c.Output
				}
			}
		}
		read := map[string]bool{}
		for _, p := range paths {
			for _, e := range p {
				l, ok := e.(*LinkEntry)
				if !ok || l.Rel != RelSubsession || l.Cancel != "" || l.Session == "" || read[l.ID] || failed[l.Session] {
					continue
				}
				read[l.ID] = true
				n := nodes[l.Session]
				if n == nil {
					child, err := resolve(l.Session)
					if child == nil || err != nil {
						switch {
						case l.URL != "" && err == nil:
							// A callee in another system whose session the
							// reader does not hold: delegated work it
							// cannot count.
							err = fmt.Errorf("%w: %s at %s", ErrChildElsewhere, l.Session, l.URL)
						case err == nil:
							err = fmt.Errorf("%w: %s", ErrChildNotFound, l.Session)
						}
						failed[l.Session] = true
						unread = append(unread, Unread{Link: l, Session: l.Session, Err: err})
						continue
					}
					n = &node{d: Delegated{Link: l, Session: l.Session}, session: child, parent: from}
					nodes[l.Session] = n
					order = append(order, l.Session)
				}
				leaf := DelegatedLeaf{Entry: n.session.Leaf()}
				if out := outputs[l.CallID]; out != nil {
					for _, r := range out.Parents {
						if r.Session == l.Session {
							leaf = DelegatedLeaf{Entry: r.Entry, Named: true}
							break
						}
					}
				}
				at := slices.IndexFunc(n.d.Leaves, func(x DelegatedLeaf) bool { return x.Entry == leaf.Entry })
				switch {
				case at >= 0:
					n.d.Leaves[at].Named = n.d.Leaves[at].Named || leaf.Named
				case n.session.Path(leaf.Entry) == nil:
					unread = append(unread, Unread{Link: l, Session: l.Session, Err: fmt.Errorf("%w: %s holds no entry %s", ErrChildNotFound, l.Session, leaf.Entry)})
				default:
					n.d.Leaves = append(n.d.Leaves, leaf)
				}
			}
		}
	}
	paths := func(n *node) [][]Entry {
		out := make([][]Entry, 0, len(n.d.Leaves))
		for _, leaf := range n.d.Leaves {
			out = append(out, n.session.Path(leaf.Entry))
		}
		return out
	}
	scan([][]Entry{path}, "")
	// A child gains a leaf when another link names it at another one, so
	// its links are read again over the union until no child gains one.
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(order); i++ {
			n := nodes[order[i]]
			if n.scanned == len(n.d.Leaves) {
				continue
			}
			n.scanned = len(n.d.Leaves)
			scan(paths(n), order[i])
			changed = true
		}
	}
	var build func(parent string) []Delegated
	build = func(parent string) []Delegated {
		var out []Delegated
		for _, id := range order {
			n := nodes[id]
			if n.parent != parent || len(n.d.Leaves) == 0 {
				continue
			}
			d := n.d
			counted := map[string]bool{}
			for _, p := range paths(n) {
				for _, c := range ModelCalls(p) {
					if eid := c.Entry.Base().ID; !counted[eid] {
						counted[eid] = true
						d.Calls = append(d.Calls, c)
					}
				}
			}
			d.Children = build(id)
			out = append(out, d)
		}
		return out
	}
	return build(""), unread
}
