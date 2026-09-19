// Package export turns session trees into linear trajectories and
// ATIF documents. The session file stays the record; an ATIF document
// is one root-to-leaf path with compaction applied, carrying the raw
// Open Responses items in its extras so nothing is lost on the way.
package export

import (
	"iter"

	"github.com/ChristopherDavenport/agentsession"
)

// Trajectory is one root-to-leaf path of a session with the context
// algorithm applied, plus what the tree says about it.
type Trajectory struct {
	// Header is the session header.
	Header agentsession.Header
	// LeafID identifies the path; it becomes the ATIF trajectory_id.
	LeafID string
	// Context is the path after compaction: settings, items and the
	// selected entries in order.
	Context agentsession.Context
	// Name is the session's display name from its info entries.
	Name string
	// Labels are the current labels of the session, by target entry.
	Labels map[string]string
	// PreferredOver lists the leaf IDs of sibling branches this path
	// was continued in preference to, at every fork it passes through
	// on the preferred side.
	PreferredOver []string
	// AbandonedAt names the fork entry at which this path left the
	// preferred route, or "" when it is a preferred path throughout.
	AbandonedAt string
	// Main is true for exactly one trajectory of a non-empty session:
	// the path ending at the most recently appended entry, which is the
	// session's current path. WriteATIF names its document after the
	// session alone, and a subsession reference that cannot be embedded
	// points at that file.
	Main bool
}

// Trajectories yields one trajectory per leaf of the session, in file
// order of the leaves. At every entry with more than one child, the
// child whose subtree holds the most recently appended entry is the
// continued branch; the others are abandoned. A leaf reached through
// an abandoned child has AbandonedAt set to the first such fork; a leaf
// on the continued side of a fork lists the abandoned subtrees' leaves
// in PreferredOver. The trajectory ending at the last appended entry is
// marked Main.
func Trajectories(s *agentsession.Session) iter.Seq2[Trajectory, error] {
	return func(yield func(Trajectory, error) bool) {
		header := s.Header()
		name := s.Name()
		labels := s.Labels()
		entries := s.Entries()
		mainLeaf := ""
		if len(entries) > 0 {
			mainLeaf = entries[len(entries)-1].Base().ID
		}
		order := make(map[string]int, len(entries))
		for i, e := range entries {
			order[e.Base().ID] = i
		}
		// latest[id] is the highest file position in id's subtree.
		latest := make(map[string]int, len(entries))
		for i := len(entries) - 1; i >= 0; i-- {
			b := entries[i].Base()
			if _, ok := latest[b.ID]; !ok {
				latest[b.ID] = i
			}
			if b.Parent != "" && latest[b.Parent] < latest[b.ID] {
				latest[b.Parent] = latest[b.ID]
			}
		}
		// leavesUnder[id] lists the leaves in id's subtree.
		leavesUnder := make(map[string][]string, len(entries))
		for i := len(entries) - 1; i >= 0; i-- {
			b := entries[i].Base()
			if len(s.Children(b.ID)) == 0 {
				leavesUnder[b.ID] = append(leavesUnder[b.ID], b.ID)
			}
			if b.Parent != "" {
				leavesUnder[b.Parent] = append(leavesUnder[b.Parent], leavesUnder[b.ID]...)
			}
		}
		for _, leaf := range s.Leaves() {
			ctx, err := s.ContextAt(leaf)
			if err != nil {
				if !yield(Trajectory{Header: header, LeafID: leaf}, err) {
					return
				}
				continue
			}
			t := Trajectory{Header: header, LeafID: leaf, Context: ctx, Name: name, Labels: labels, Main: leaf == mainLeaf}
			for _, e := range s.Path(leaf) {
				b := e.Base()
				children := s.Children(b.ID)
				if len(children) < 2 {
					continue
				}
				preferred := children[0]
				for _, c := range children[1:] {
					if latest[c] > latest[preferred] {
						preferred = c
					}
				}
				next := nextOnPath(s, leaf, b.ID)
				if next == preferred {
					for _, c := range children {
						if c != preferred {
							t.PreferredOver = append(t.PreferredOver, leavesUnder[c]...)
						}
					}
				} else if t.AbandonedAt == "" {
					t.AbandonedAt = b.ID
				}
			}
			if !yield(t, nil) {
				return
			}
		}
	}
}

// nextOnPath returns the child of parent that lies on the path to leaf.
func nextOnPath(s *agentsession.Session, leaf, parent string) string {
	for id := leaf; id != ""; {
		e, ok := s.Entry(id)
		if !ok {
			return ""
		}
		if e.Base().Parent == parent {
			return id
		}
		id = e.Base().Parent
	}
	return ""
}
