// Package export turns session trees into linear trajectories and
// ATIF documents. The session file stays the record; an ATIF document
// is one root-to-leaf path with compaction applied, carrying the raw
// Open Responses items in its extras so nothing is lost on the way.
package export

import (
	"iter"
	"slices"

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

// A Preference chooses the continued child at a fork: fork is the
// entry with more than one child and children are its children in
// file order. It returns one of the children, or "" to express no
// opinion. [Trajectories] asks each preference in turn and falls back
// to [PreferLatest].
type Preference func(s *agentsession.Session, fork string, children []string) string

// PreferLatest is the default rule: the child whose subtree holds the
// most recently appended entry was continued. It is right when a user
// abandons a branch and moves on, and wrong when they hop between
// branches.
func PreferLatest(s *agentsession.Session, fork string, children []string) string {
	entries := s.Entries()
	order := make(map[string]int, len(entries))
	for i, e := range entries {
		order[e.Base().ID] = i
	}
	best, bestAt := "", -1
	for _, c := range children {
		for id := range subtree(s, c) {
			if at := order[id]; at > bestAt {
				best, bestAt = c, at
			}
		}
	}
	return best
}

// PreferCurrentLeaf chooses the child whose subtree holds the session's
// current leaf, so the branch a user has switched back to counts as
// continued. It has no opinion at forks the leaf is not under.
func PreferCurrentLeaf(s *agentsession.Session, fork string, children []string) string {
	return childHolding(s, children, s.Leaf())
}

// PreferLabel chooses the child whose subtree holds an entry carrying
// the label, for a harness that marks the branch it kept. It has no
// opinion when no child, or more than one, is labelled.
func PreferLabel(label string) Preference {
	return func(s *agentsession.Session, fork string, children []string) string {
		var targets []string
		for target, l := range s.Labels() {
			if l == label {
				targets = append(targets, target)
			}
		}
		chosen := ""
		for _, target := range targets {
			c := childHolding(s, children, target)
			if c == "" || c == chosen {
				continue
			}
			if chosen != "" {
				return ""
			}
			chosen = c
		}
		return chosen
	}
}

// PreferScore chooses the child whose subtree holds the outcome with
// the highest score. An outcome counts for the entry it targets, or
// for its own position when it has no target. It has no opinion when
// no child has a scored outcome, or when the best scores tie.
func PreferScore(s *agentsession.Session, fork string, children []string) string {
	best := map[string]float64{}
	for _, e := range s.Entries() {
		o, ok := e.(*agentsession.OutcomeEntry)
		if !ok || o.Score == nil {
			continue
		}
		at := o.Target
		if at == "" {
			at = o.ID
		}
		c := childHolding(s, children, at)
		if c == "" {
			continue
		}
		if cur, ok := best[c]; !ok || *o.Score > cur {
			best[c] = *o.Score
		}
	}
	chosen, top, tie := "", 0.0, false
	for _, c := range children {
		score, ok := best[c]
		switch {
		case !ok:
		case chosen == "" || score > top:
			chosen, top, tie = c, score, false
		case score == top:
			tie = true
		}
	}
	if tie {
		return ""
	}
	return chosen
}

// childHolding returns the child whose subtree contains id, or "".
func childHolding(s *agentsession.Session, children []string, id string) string {
	if id == "" {
		return ""
	}
	on := map[string]bool{}
	for _, e := range s.Path(id) {
		on[e.Base().ID] = true
	}
	for _, c := range children {
		if on[c] {
			return c
		}
	}
	return ""
}

// subtree yields the IDs of root and every entry below it.
func subtree(s *agentsession.Session, root string) iter.Seq[string] {
	return func(yield func(string) bool) {
		stack := []string{root}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !yield(id) {
				return
			}
			stack = append(stack, s.Children(id)...)
		}
	}
}

// Trajectories yields one trajectory per leaf of the session, in file
// order of the leaves. At every entry with more than one child, one
// child is the continued branch and the others are abandoned: the
// preferences are asked in order and the first that names a child
// decides; with none, or when none has an opinion, [PreferLatest]
// applies. A leaf reached through an abandoned child has AbandonedAt
// set to the first such fork; a leaf on the continued side of a fork
// lists the abandoned subtrees' leaves in PreferredOver. The
// trajectory ending at the last appended entry is marked Main whatever
// the preferences decide.
func Trajectories(s *agentsession.Session, prefs ...Preference) iter.Seq2[Trajectory, error] {
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
				preferred := ""
				for _, p := range prefs {
					if preferred = p(s, b.ID, children); slices.Contains(children, preferred) {
						break
					}
					preferred = ""
				}
				if preferred == "" {
					preferred = children[0]
					for _, c := range children[1:] {
						if latest[c] > latest[preferred] {
							preferred = c
						}
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
