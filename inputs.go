package agentsession

import "github.com/ChristopherDavenport/openresponses"

// Why a user message on a path is not an input that started a turn
// (RFC 0001, run, since 0.17), as [PathInputs] reports it.
const (
	// NotInputSteer is an item drained from a queued steer, a delivery
	// among them, after its run's first response: it joined the run in
	// flight. On a path with no run entries every such item is one,
	// since a reader cannot tell one that started a turn.
	NotInputSteer = "steer"
	// NotInputCopied is an item carrying copied_from, which another
	// session's conversation holds.
	NotInputCopied = "copied"
	// NotInputPeerReport is an item carrying from_link, a peer's report.
	NotInputPeerReport = "from_link"
	// NotInputComposed is an item carrying instructions_part, which the
	// harness composed from one instruction layer or several.
	NotInputComposed = "instructions_part"
	// NotInputResume is a user message on a resume run's segment, such
	// as the note a person typed with an approval.
	NotInputResume = "resume"
	// NotInputMidRun is a user message on an input run's segment after
	// its first response that none of the above names: it reached the
	// model mid-run, and is no input of a new turn.
	NotInputMidRun = "mid_run"
)

// LeftOutInput is a user message a reader rebuilding a path's inputs
// leaves out, and why: one of the NotInput kinds.
type LeftOutInput struct {
	Entry *ItemEntry
	Kind  string
}

// TurnInputs are the inputs of one turn of a path: a run's, or on a path
// with no run entries the user messages up to a response.
type TurnInputs struct {
	// Run is the run, or nil on a path with no run entries.
	Run *Run
	// Inputs are the user messages that drove the turn, in path order.
	Inputs []*ItemEntry
	// LeftOut are the other user messages of the turn, in path order. A
	// reader that cannot reproduce one where it stood, a steer mid-run or
	// a delivery above all, says so rather than send it as the input of a
	// new turn.
	LeftOut []LeftOutInput
}

// PathInputs returns the inputs that drove a root-first path, as a
// replay that sends them again rebuilds them (RFC 0001, run, since
// 0.17), one TurnInputs per run, and byRuns true. A run whose source is
// input takes as its inputs the user messages of its segment before its
// first response, all of them when it has none, leaving out one
// carrying copied_from, from_link or instructions_part. An item drained
// from a queued entry before that response, a steer, a delivery or a
// followup, is an input of the run: a delivery that starts a run is its
// input. One drained after it joined the run in flight. Every user
// message of a resume run belongs to the resume and is left out.
//
// On a path with no run entries, byRuns is false and each turn holds
// the user messages up to a response, those after the last included,
// leaving out the same items and every item drained from a steer, which
// it cannot tell from one that started a turn; the caller then says it
// could not tell a resume's note, or a delivery that started a run,
// from what it took.
func PathInputs(path []Entry) (turns []TurnInputs, byRuns bool) {
	queued := map[string]*QueuedEntry{}
	for _, e := range path {
		if q, ok := e.(*QueuedEntry); ok {
			queued[q.ID] = q
		}
	}
	steered := func(it *ItemEntry) bool {
		q := queued[it.QueuedFrom]
		return it.QueuedFrom != "" && q != nil && q.Mode == ModeSteer
	}
	kind := func(it *ItemEntry) string {
		switch {
		case it.CopiedFrom != nil:
			return NotInputCopied
		case it.FromLink != "":
			return NotInputPeerReport
		case it.Layers() != nil:
			return NotInputComposed
		}
		return ""
	}
	if runs := Runs(path); len(runs) > 0 {
		for _, r := range runs {
			turn := TurnInputs{Run: r}
			answered := false
			for _, e := range r.Segment {
				if _, ok := e.(*ResponseEntry); ok {
					answered = true
					continue
				}
				it := userMessage(e)
				if it == nil {
					continue
				}
				k := kind(it)
				switch {
				case r.Start.Source != SourceInput:
					k = NotInputResume
				case k == "" && answered && steered(it):
					k = NotInputSteer
				case k == "" && answered:
					k = NotInputMidRun
				}
				if k == "" {
					turn.Inputs = append(turn.Inputs, it)
				} else {
					turn.LeftOut = append(turn.LeftOut, LeftOutInput{Entry: it, Kind: k})
				}
			}
			turns = append(turns, turn)
		}
		return turns, true
	}
	var turn TurnInputs
	for _, e := range path {
		if _, ok := e.(*ResponseEntry); ok {
			if len(turn.Inputs)+len(turn.LeftOut) > 0 {
				turns = append(turns, turn)
			}
			turn = TurnInputs{}
			continue
		}
		it := userMessage(e)
		if it == nil {
			continue
		}
		k := kind(it)
		if k == "" && steered(it) {
			k = NotInputSteer
		}
		if k != "" {
			turn.LeftOut = append(turn.LeftOut, LeftOutInput{Entry: it, Kind: k})
		} else {
			turn.Inputs = append(turn.Inputs, it)
		}
	}
	if len(turn.Inputs)+len(turn.LeftOut) > 0 {
		turns = append(turns, turn)
	}
	return turns, false
}

// userMessage returns e when it is an item entry holding a message of
// role user, and nil otherwise.
func userMessage(e Entry) *ItemEntry {
	it, ok := e.(*ItemEntry)
	if !ok {
		return nil
	}
	if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
		return it
	}
	return nil
}
