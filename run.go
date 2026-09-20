package agentsession

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ChristopherDavenport/openresponses"
)

// Call is one function call on a path and everything that happened to
// it there: the decisions made about it, its dispatch and its output.
type Call struct {
	// Entry is the item entry holding the function call.
	Entry *ItemEntry
	// Call is the function call item.
	Call *openresponses.FunctionCall
	// Decisions are the decisions on the call, in path order.
	Decisions []*DecisionEntry
	// Dispatch is the dispatch entry, or nil when none is on the path.
	Dispatch *DispatchEntry
	// Output is the item entry holding the function call output, or nil
	// when the call is pending.
	Output *ItemEntry

	dispatchAfterReject bool
}

// ID returns the call ID.
func (c *Call) ID() string { return c.Call.CallID }

// Pending reports whether the call has no output on the path.
func (c *Call) Pending() bool { return c.Output == nil }

// Held reports whether the call is waiting on an answer: its latest
// decision is a hold and no dispatch follows it.
func (c *Call) Held() bool {
	return c.Dispatch == nil && len(c.Decisions) > 0 && c.Decisions[len(c.Decisions)-1].Verdict == VerdictHold
}

// Rejected reports whether a decision ended the call without running
// it.
func (c *Call) Rejected() bool {
	for _, d := range c.Decisions {
		if d.Verdict == VerdictReject {
			return true
		}
	}
	return false
}

// Args returns the arguments the tool ran with: those of the last
// decision that rewrote them, else the call's own.
func (c *Call) Args() string {
	for i := len(c.Decisions) - 1; i >= 0; i-- {
		if len(c.Decisions[i].Args) > 0 {
			return string(c.Decisions[i].Args)
		}
	}
	return c.Call.Arguments
}

// CallState is what the path says happened to a call.
type CallState int

const (
	// CallCompleted: the call has an output.
	CallCompleted CallState = iota
	// CallHeld: the call waits on an answer to a hold.
	CallHeld
	// CallInFlight: the call was dispatched and no output arrived, so
	// its side effect may have happened.
	CallInFlight
	// CallNeverStarted: no dispatch and no output, in a file whose
	// header promises dispatches are recorded.
	CallNeverStarted
	// CallUnknown: no dispatch and no output, in a file that makes no
	// such promise, so the file does not say whether the tool ran.
	CallUnknown
)

// String names the state.
func (s CallState) String() string {
	switch s {
	case CallCompleted:
		return "completed"
	case CallHeld:
		return "held"
	case CallInFlight:
		return "in_flight"
	case CallNeverStarted:
		return "never_started"
	case CallUnknown:
		return "unknown"
	}
	return fmt.Sprintf("CallState(%d)", int(s))
}

// State returns what the path says happened to the call, reading the
// header's records to decide whether a missing dispatch means the
// call never started or means the file does not say.
func (c *Call) State(h Header) CallState {
	switch {
	case c.Output != nil:
		return CallCompleted
	case c.Held():
		return CallHeld
	case c.Dispatch != nil:
		return CallInFlight
	case h.HasRecord(TypeDispatch):
		return CallNeverStarted
	}
	return CallUnknown
}

// Calls collects the function calls on a root-first path, in the order
// their items appear, with the decisions, dispatch and output the path
// holds for each. A decision, dispatch or output whose call is not on
// the path is ignored.
func Calls(path []Entry) []*Call {
	var out []*Call
	byID := map[string]*Call{}
	for _, e := range path {
		switch v := e.(type) {
		case *ItemEntry:
			switch it := v.Item.(type) {
			case *openresponses.FunctionCall:
				if _, seen := byID[it.CallID]; seen {
					continue
				}
				c := &Call{Entry: v, Call: it}
				byID[it.CallID] = c
				out = append(out, c)
			case *openresponses.FunctionCallOutput:
				if c, ok := byID[it.CallID]; ok && c.Output == nil {
					c.Output = v
				}
			}
		case *DecisionEntry:
			if c, ok := byID[v.CallID]; ok {
				c.Decisions = append(c.Decisions, v)
			}
		case *DispatchEntry:
			if c, ok := byID[v.CallID]; ok && c.Dispatch == nil {
				c.Dispatch = v
				c.dispatchAfterReject = c.Rejected()
			}
		}
	}
	return out
}

// Calls returns the calls on the path to leaf; see [Calls].
func (s *Session) Calls(leaf string) ([]*Call, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	return Calls(path), nil
}

// PendingCalls returns the calls on the path to leaf that have no
// output, which is what a resume answers.
func (s *Session) PendingCalls(leaf string) ([]*Call, error) {
	calls, err := s.Calls(leaf)
	if err != nil {
		return nil, err
	}
	var out []*Call
	for _, c := range calls {
		if c.Pending() {
			out = append(out, c)
		}
	}
	return out, nil
}

// Run is one run's segment of a path: the entries from its start entry
// to its end entry, or to the end of the path when the run was cut off
// or closed by a branch.
type Run struct {
	Start *RunEntry
	// End is nil when no end entry for the run is on the segment.
	End     *RunEntry
	Segment []Entry
}

// RunID returns the run's ID.
func (r *Run) RunID() string { return r.Start.RunID }

// Calls returns the calls on the segment; see [Calls].
func (r *Run) Calls() []*Call { return Calls(r.Segment) }

// Pending returns the IDs of the calls on the segment with no output
// on it.
func (r *Run) Pending() []string {
	var out []string
	for _, c := range r.Calls() {
		if c.Pending() {
			out = append(out, c.ID())
		}
	}
	return out
}

// Runs partitions a root-first path into its runs. Entries before the
// first start entry belong to no run and are dropped; a start entry
// closes any run still open, as a branch does. Only an end entry whose
// run_id matches the open run closes it.
func Runs(path []Entry) []*Run {
	var runs []*Run
	var cur *Run
	for _, e := range path {
		if r, ok := e.(*RunEntry); ok && r.IsStart() {
			cur = &Run{Start: r}
			runs = append(runs, cur)
		}
		if cur == nil {
			continue
		}
		if cur.End != nil {
			// Entries after an end belong to no run until the next start.
			cur = nil
			continue
		}
		cur.Segment = append(cur.Segment, e)
		if r, ok := e.(*RunEntry); ok && r.IsEnd() && r.RunID == cur.Start.RunID {
			cur.End = r
		}
	}
	return runs
}

// Runs returns the runs on the path to leaf; see [Runs].
func (s *Session) Runs(leaf string) ([]*Run, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	return Runs(path), nil
}

// OpenRun returns the run on the path to leaf that has no end entry,
// or nil when every run is closed or there is none.
func (s *Session) OpenRun(leaf string) (*Run, error) {
	runs, err := s.Runs(leaf)
	if err != nil {
		return nil, err
	}
	if n := len(runs); n > 0 && runs[n-1].End == nil {
		return runs[n-1], nil
	}
	return nil, nil
}

// EndRun builds the end entry for the run open at the current leaf:
// its pending list is the calls on the segment with no output. reason
// is one of the Reason constants and ref may be "". The entry is not
// appended.
func (s *Session) EndRun(reason, ref string) (*RunEntry, error) {
	run, err := s.OpenRun(s.Leaf())
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, errors.New("agentsession: no open run at the leaf")
	}
	return NewRunEnd(run.RunID(), reason, ref, run.Pending()), nil
}

// ComputeReason recomputes a run's end reason from its segment, as the
// format defines it: the first of error, input_required, aborted, done
// and stopped whose shape the segment matches. It never returns
// ReasonInterrupted, and returns ReasonError only for a response that
// carries an error; both are values a writer adds where the segment
// cannot show them.
func ComputeReason(segment []Entry) string {
	var last *ResponseEntry
	for _, e := range segment {
		if r, ok := e.(*ResponseEntry); ok {
			last = r
		}
	}
	if last != nil && last.Error != nil {
		return ReasonError
	}
	calls := Calls(segment)
	held, dispatched, pending := false, false, false
	for _, c := range calls {
		if !c.Pending() {
			continue
		}
		pending = true
		if c.Held() {
			held = true
		}
		if c.Dispatch != nil {
			dispatched = true
		}
	}
	if held && !dispatched {
		return ReasonInputRequired
	}
	if pending || last == nil || last.Incomplete != nil {
		return ReasonAborted
	}
	for _, c := range calls {
		if c.Entry.ResponseID == last.ResponseID {
			return ReasonStopped
		}
	}
	return ReasonDone
}

// ErrReasonMismatch is returned by [Run.Verify] when a run's written
// end reason or pending list disagrees with its segment.
var ErrReasonMismatch = errors.New("agentsession: run end disagrees with its segment")

// Verify checks the run's end entry against its segment. A written
// error or interrupted stands over any segment; any other reason must
// match [ComputeReason], and the pending list must match the calls on
// the segment without an output. A run without an end verifies
// trivially.
func (r *Run) Verify() error {
	if r.End == nil {
		return nil
	}
	if r.End.Reason != ReasonError && r.End.Reason != ReasonInterrupted {
		if got := ComputeReason(r.Segment); got != r.End.Reason {
			return fmt.Errorf("%w: run %s wrote %s, segment reads %s", ErrReasonMismatch, r.RunID(), r.End.Reason, got)
		}
	}
	want := r.Pending()
	got := append([]string(nil), r.End.Pending...)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return fmt.Errorf("%w: run %s wrote pending %v, segment has %v", ErrReasonMismatch, r.RunID(), r.End.Pending, want)
	}
	return nil
}

// ErrCallRejected is returned when a dispatch is appended for a call
// that a decision on the path already rejected.
var ErrCallRejected = errors.New("agentsession: call was rejected")

// ErrRecordMissing is returned by [Session.VerifyRecords] when the
// header promises a record entry type and the path lacks one where the
// event plainly happened.
var ErrRecordMissing = errors.New("agentsession: promised record entry missing")

// VerifyRecords checks the record entries on the path to leaf against
// the format's rules: every run end agrees with its segment, no
// dispatch follows a reject on the same call, and, when the header
// names dispatch in records, every call that ran has a dispatch. It
// returns the first problem found.
func (s *Session) VerifyRecords(leaf string) error {
	path := s.Path(leaf)
	if path == nil {
		return fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	for _, r := range Runs(path) {
		if err := r.Verify(); err != nil {
			return err
		}
	}
	h := s.Header()
	for _, c := range Calls(path) {
		if c.dispatchAfterReject {
			return fmt.Errorf("%w: dispatch %s follows a reject of call %s", ErrCallRejected, c.Dispatch.ID, c.ID())
		}
		if h.HasRecord(TypeDispatch) && c.Output != nil && c.Dispatch == nil && !c.Rejected() {
			return fmt.Errorf("%w: call %s has an output and no dispatch", ErrRecordMissing, c.ID())
		}
	}
	return nil
}
