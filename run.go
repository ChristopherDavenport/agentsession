package agentsession

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ChristopherDavenport/openresponses"
)

// Call is one function call on a path and everything that happened to
// it there: the decisions made about it, its dispatches and its output.
type Call struct {
	// Entry is the item entry holding the function call.
	Entry *ItemEntry
	// Call is the function call item.
	Call *openresponses.FunctionCall
	// Decisions are the decisions on the call, in path order.
	Decisions []*DecisionEntry
	// Dispatch is the first dispatch entry, or nil when none is on the
	// path.
	Dispatch *DispatchEntry
	// Dispatches are the dispatch entries, in path order: one for each
	// time the call was handed to its tool, so a call run again after a
	// restart has two. Dispatch is the first of them.
	Dispatches []*DispatchEntry
	// Output is the item entry holding the function call output, or nil
	// when the call is pending.
	Output *ItemEntry

	// inPrefix is set by the session for a call made in a fork's
	// prefix, which its header's promise does not cover.
	inPrefix bool

	// ended is the decision that ended the call, a reject or an answer,
	// that a dispatch follows on the path, which the format forbids.
	ended *DecisionEntry
	// endedBy is the dispatch that follows it.
	endedBy *DispatchEntry
	// sinceDecision reports whether a dispatch follows the latest
	// decision.
	sinceDecision bool
	// dispatchedArgs are the arguments the last dispatch handed the
	// tool.
	dispatchedArgs string
	// afterAnswer and afterReject are the first decisions that follow
	// an answer and a reject, endAfterOutput the first answer or reject
	// that follows the output, and rejectAfterDispatch the first reject
	// that follows a dispatch; the format forbids all four.
	// dispatchAfterOutput is the first dispatch that follows the
	// output, which it forbids too.
	dispatchAfterOutput                                           *DispatchEntry
	afterAnswer, afterReject, endAfterOutput, rejectAfterDispatch *DecisionEntry
}

// ID returns the call ID.
func (c *Call) ID() string { return c.Call.CallID }

// Pending reports whether the call has no output on the path.
func (c *Call) Pending() bool { return c.Output == nil }

// Held reports whether the call is waiting on an answer: its latest
// decision is a hold and no dispatch follows it. A hold after a
// dispatch is a call that may have run and waits on someone to say
// whether it runs again; it is held, and its Dispatches say it may
// have run.
func (c *Call) Held() bool { return c.latestIs(VerdictHold) }

// InFlight reports whether the call was handed to its tool when the
// record stopped: it has a dispatch and no output, and no decision
// after its last dispatch holds it or answers it. Its side effect may
// have happened.
func (c *Call) InFlight() bool {
	return c.Output == nil && len(c.Dispatches) > 0 && !c.latestIs(VerdictHold) && !c.latestIs(VerdictAnswer)
}

// latestIs reports whether the latest decision has the verdict and no
// dispatch follows it.
func (c *Call) latestIs(verdict string) bool {
	return !c.sinceDecision && len(c.Decisions) > 0 && c.Decisions[len(c.Decisions)-1].Verdict == verdict
}

// Rejected reports whether a decision ended the call without running
// it.
func (c *Call) Rejected() bool { return c.hasVerdict(VerdictReject) }

// Answered reports whether a decision ended the call with an output the
// harness wrote rather than one its tool returned: a call that may have
// run, answered without being handed to its tool again.
func (c *Call) Answered() bool { return c.hasVerdict(VerdictAnswer) }

func (c *Call) hasVerdict(verdict string) bool {
	for _, d := range c.Decisions {
		if d.Verdict == verdict {
			return true
		}
	}
	return false
}

// IdempotencyKey returns the key the call's last dispatch handed its
// tool, or "" when it has no dispatch or the dispatch no key. It is the
// key a further run of that hand-off repeats, and it pairs with
// [Call.DispatchedArgs]: a harness that ran the call again under a new
// key, which it does for new arguments, made a new operation, and the
// key of an earlier dispatch describes the earlier arguments.
func (c *Call) IdempotencyKey() string {
	if len(c.Dispatches) == 0 {
		return ""
	}
	return c.Dispatches[len(c.Dispatches)-1].IdempotencyKey
}

// DispatchedArgs returns the arguments the call's last dispatch handed
// its tool: those of the last decision before it that rewrote them,
// else the call's own. It is "" when the call has no dispatch. A
// replay rule deciding whether that hand-off may run again reads these
// beside [Call.IdempotencyKey]; [Call.Args] are the arguments a new
// hand-off would run with, which a later decision may have changed.
func (c *Call) DispatchedArgs() string {
	if len(c.Dispatches) == 0 {
		return ""
	}
	return c.dispatchedArgs
}

// From returns the predecessors the call's output converges: for a
// call a subagent answered, the leaf of the child session the answer
// was taken from, which the format says the output entry SHOULD name
// and which is the first moment the parent knows it. It is nil for a
// pending call, and for an output whose writer recorded no provenance.
//
// A [LinkEntry] names the child session and is written when the call
// is dispatched, before the child has a point to name; this is the
// other half of that round trip. Without it, a child that branched
// leaves no record of which of its leaves answered, and a projection
// has to choose.
func (c *Call) From() []EntryRef {
	if c.Output == nil {
		return nil
	}
	return c.Output.Parents
}

// Args returns the arguments the tool runs with: those of the last
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
	// CallNeverStarted: no dispatch and no output, for a call the
	// session made under a header that promises dispatches are recorded
	// before the tool runs, so the tool never ran.
	// A call in a fork's prefix was made under its origin's promise,
	// which this header does not vouch for, and is CallUnknown.
	CallNeverStarted
	// CallUnknown: no dispatch and no output, where no promise covers
	// the call, so the file does not say whether the tool ran.
	CallUnknown
	// CallAnswered: an answer decision ended the call and its output is
	// not on the path yet, since the record stopped between the two.
	// The harness that continues the path writes the output and
	// nothing else for the call; no dispatch may follow.
	CallAnswered
	// CallRejected: a reject decision ended the call and its refusal
	// output is not on the path yet, since the record stopped between
	// the two. The call did not run and does not: the harness that
	// continues the path writes the output, carrying the reject's
	// reason, and nothing else for the call.
	CallRejected
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
	case CallAnswered:
		return "answered"
	case CallRejected:
		return "rejected"
	}
	return fmt.Sprintf("CallState(%d)", int(s))
}

// State returns what the path says happened to the call, reading the
// header's records to decide whether a missing dispatch means the
// call never started or means the file does not say. A held call with
// dispatches is held, and may have run. The promise covers a call the
// session made, after its base: one in a fork's prefix, from a call
// [Session.Calls] or [Session.PendingCalls] returns, is unknown, since
// its origin may have promised nothing. A call from [Calls] over a
// bare path is taken to be the session's own.
func (c *Call) State(h Header) CallState {
	switch {
	case c.Output != nil:
		return CallCompleted
	case c.Answered():
		return CallAnswered
	case c.Rejected():
		return CallRejected
	case c.Held():
		return CallHeld
	case c.Dispatch != nil:
		return CallInFlight
	case h.HasRecord(TypeDispatch) && !c.inPrefix:
		return CallNeverStarted
	}
	return CallUnknown
}

// Calls collects the function calls on a root-first path, in the order
// their items appear, with the decisions, dispatch and output the path
// holds for each. A decision or dispatch belongs to the call its
// target names, an output to the latest call before it with its call
// ID, as the format has it; one whose call is not on the path is
// ignored.
func Calls(path []Entry) []*Call {
	var out []*Call
	// byID is the latest call with each call ID, byEntry the call each
	// function call entry holds. A decision or dispatch names its call
	// by target; one whose target is no call on the path, which the
	// format forbids, falls back to its call ID.
	byID := map[string]*Call{}
	byEntry := map[string]*Call{}
	bind := func(callID, target string) *Call {
		if c, ok := byEntry[target]; ok {
			return c
		}
		return byID[callID]
	}
	for _, e := range path {
		switch v := e.(type) {
		case *ItemEntry:
			switch it := v.Item.(type) {
			case *openresponses.FunctionCall:
				// The format forbids a repeated call ID; in a file that
				// repeats one, an output names the latest call with it.
				c := &Call{Entry: v, Call: it}
				byID[it.CallID] = c
				byEntry[v.ID] = c
				out = append(out, c)
			case *openresponses.FunctionCallOutput:
				if c, ok := byID[it.CallID]; ok && c.Output == nil {
					c.Output = v
				}
			}
		case *DecisionEntry:
			if c := bind(v.CallID, v.Target); c != nil {
				if c.afterAnswer == nil && c.Answered() {
					c.afterAnswer = v
				}
				if c.afterReject == nil && c.Rejected() {
					c.afterReject = v
				}
				if c.rejectAfterDispatch == nil && v.Verdict == VerdictReject && c.Dispatch != nil {
					c.rejectAfterDispatch = v
				}
				if c.endAfterOutput == nil && (v.Verdict == VerdictAnswer || v.Verdict == VerdictReject) && c.Output != nil {
					c.endAfterOutput = v
				}
				c.Decisions = append(c.Decisions, v)
				c.sinceDecision = false
			}
		case *DispatchEntry:
			if c := bind(v.CallID, v.Target); c != nil {
				if c.dispatchAfterOutput == nil && c.Output != nil {
					c.dispatchAfterOutput = v
				}
				if c.Dispatch == nil {
					c.Dispatch = v
				}
				c.Dispatches = append(c.Dispatches, v)
				c.dispatchedArgs = c.Args()
				c.sinceDecision = true
				if c.ended == nil {
					c.ended = c.endingDecision()
					if c.ended != nil {
						c.endedBy = v
					}
				}
			}
		}
	}
	return out
}

// endingDecision returns the first decision that ended the call, a
// reject or an answer, or nil.
func (c *Call) endingDecision() *DecisionEntry {
	for _, d := range c.Decisions {
		if d.Verdict == VerdictReject || d.Verdict == VerdictAnswer {
			return d
		}
	}
	return nil
}

// Calls returns the calls on the path to leaf; see [Calls]. A call
// made in a fork's prefix is known as one, so its State reads the
// header's promise as not covering it.
func (s *Session) Calls(leaf string) ([]*Call, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	calls := Calls(path)
	for _, c := range calls {
		c.inPrefix = s.Prefix(c.Entry.ID)
	}
	return calls, nil
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

// Queued returns the queued entries on a root-first path that are
// still waiting: no item entry on the path names them in QueuedFrom,
// and no run end follows them. They are the inputs a harness accepted
// and has not yet appended to the conversation, the durable inbox a
// resume drains. A run end after a queued entry closes it, since the
// run it was queued into has ended: a harness that still wants the
// input queues it again.
func Queued(path []Entry) []*QueuedEntry {
	var open []*QueuedEntry
	drained := map[string]bool{}
	for _, e := range path {
		switch v := e.(type) {
		case *QueuedEntry:
			open = append(open, v)
		case *ItemEntry:
			if v.QueuedFrom != "" {
				drained[v.QueuedFrom] = true
			}
		case *RunEntry:
			if v.IsEnd() {
				open = nil
			}
		}
	}
	var out []*QueuedEntry
	for _, q := range open {
		if !drained[q.ID] {
			out = append(out, q)
		}
	}
	return out
}

// PendingQueued returns the inputs queued on the path to leaf that
// have not been appended; see [Queued].
func (s *Session) PendingQueued(leaf string) ([]*QueuedEntry, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	return Queued(path), nil
}

// Run is one run's segment of a path: the entries from its start entry
// to its end entry, or to the end of the path when the run was cut off
// or closed by a branch.
type Run struct {
	Start *RunEntry
	// End is nil when no end entry for the run is on the segment.
	End     *RunEntry
	Segment []Entry
	// Path is the root-first path up to the last entry of the segment,
	// of which Segment is the tail. The end reason of a run that
	// answers a call an earlier run's model call made is a shape of
	// the path, not of the segment alone; see [ComputeReason]. It is
	// nil in a Run built by hand, and then the segment stands for the
	// path.
	Path []Entry
}

// RunID returns the run's ID.
func (r *Run) RunID() string { return r.Start.RunID }

// Calls returns the run's calls: those on its segment, and those made
// before it that the segment holds a decision, a dispatch or an output
// for, since the run took them up. Each carries what the path holds
// for it; see [Calls].
func (r *Run) Calls() []*Call { return runCalls(r.Path, r.Segment) }

// runCalls returns the calls of the run whose segment ends the path:
// the calls on the path whose function call, or a decision, dispatch
// or output bound to them, is on the segment. A nil path means the
// segment stands for the path.
func runCalls(path, segment []Entry) []*Call {
	if path == nil {
		path = segment
	}
	in := map[string]bool{}
	for _, e := range segment {
		if id := e.Base().ID; id != "" {
			in[id] = true
		}
	}
	var out []*Call
	for _, c := range Calls(path) {
		if c.takenUpIn(in) {
			out = append(out, c)
		}
	}
	return out
}

// takenUpIn reports whether the call's function call, or a decision,
// dispatch or output bound to it, is one of the entries.
func (c *Call) takenUpIn(in map[string]bool) bool {
	if in[c.Entry.ID] || (c.Output != nil && in[c.Output.ID]) {
		return true
	}
	for _, d := range c.Decisions {
		if in[d.ID] {
			return true
		}
	}
	for _, d := range c.Dispatches {
		if in[d.ID] {
			return true
		}
	}
	return false
}

// ComputeSource returns the source a run's start names, by the
// format's rule: [SourceResume] when the segment's first function call
// output, decision or dispatch takes up a call that was on the path
// with no output when the run began, whatever messages come before
// it, and [SourceInput] otherwise, a run that takes up nothing
// included. A run built by hand with no Path has nothing before its
// segment, so it is an input.
func ComputeSource(r *Run) string {
	path := r.Path
	if path == nil {
		path = r.Segment
	}
	before := map[string]bool{}
	for _, e := range path[:len(path)-len(r.Segment)] {
		before[e.Base().ID] = true
	}
	// part names the call each output, decision and dispatch is for.
	part := map[string]*Call{}
	for _, c := range Calls(path) {
		if c.Output != nil {
			part[c.Output.ID] = c
		}
		for _, d := range c.Decisions {
			part[d.ID] = c
		}
		for _, d := range c.Dispatches {
			part[d.ID] = c
		}
	}
	for _, e := range r.Segment {
		c, ok := part[e.Base().ID]
		if !ok {
			// An output no call on the path is owed takes up nothing,
			// and is still the segment's first output.
			if it, isItem := e.(*ItemEntry); isItem {
				if _, isOut := it.Item.(*openresponses.FunctionCallOutput); isOut {
					return SourceInput
				}
			}
			continue
		}
		if before[c.Entry.ID] && (c.Output == nil || !before[c.Output.ID]) {
			return SourceResume
		}
		return SourceInput
	}
	return SourceInput
}

// Pending returns the IDs of the run's calls with no output on the
// path, a call made before the segment that the run took up among them;
// see [Run.Calls].
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
	for i, e := range path {
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
		// Capped, so appending to one run's path cannot overwrite the
		// entry the next run's path starts from.
		cur.Path = path[: i+1 : i+1]
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
// its pending list is [Run.Pending]. reason is one of the Reason
// constants and ref may be "". The entry is not appended.
//
// A writer that continues a path on which a run is open that it is not
// running owns that run and closes it before appending anything else,
// as the format has it: after [Session.Branch] into a run, with
// ReasonInterrupted and a ref naming the rewind; on resuming a run a
// crash cut off, with ReasonError and a ref naming the cut.
// [Session.OpenRun] at the leaf says whether there is one.
func (s *Session) EndRun(reason, ref string) (*RunEntry, error) {
	run, err := s.OpenRun(s.Leaf())
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, errors.New("agentsession: no open run at the leaf")
	}
	if reason == "" {
		return nil, errors.New("agentsession: a run end needs a reason")
	}
	return NewRunEnd(run.RunID(), reason, ref, run.Pending()), nil
}

// ComputeReason recomputes a run's end reason from its segment and
// the root-first path the segment ends, as the format defines it: the
// first of error, input_required, aborted, done and stopped whose
// shape they match, with aborted again as the value for anything left.
// A nil path means the segment stands for the path. It never returns
// ReasonInterrupted, and returns ReasonError only for a response that
// carries an error; both are values a writer adds where the segment
// cannot show them.
//
// A pending call is one of the run's calls, as [Run.Calls] has them,
// with no output: a call made before the segment counts once the
// segment holds a decision, dispatch or output for it, so a resume
// that holds such a call before any model call ends input_required.
//
// The stopped step reads the path because a run that answers a call
// and ends without calling the model again, which is what a resume
// whose tool asks to terminate and a refusal both are, holds no
// response of its own: the response that made the calls is on the
// path before the segment.
func ComputeReason(path, segment []Entry) string {
	if path == nil {
		path = segment
	}
	last := lastResponse(segment)
	if last != nil && last.Error != nil {
		return ReasonError
	}
	calls := runCalls(path, segment)
	onPath := Calls(path)
	held, dispatched, pending := false, false, false
	for _, c := range calls {
		if !c.Pending() {
			continue
		}
		pending = true
		if c.Held() {
			held = true
		}
		if c.InFlight() {
			dispatched = true
		}
	}
	if held && !dispatched {
		return ReasonInputRequired
	}
	if pending || (last != nil && last.Incomplete != nil) {
		return ReasonAborted
	}
	// Whether the model asked for a tool is a property of the
	// response's own output items, which the path holds wherever they
	// were written: a run that starts between a function call and its
	// response has them outside the segment.
	if last != nil && !madeCalls(onPath, last) {
		return ReasonDone
	}
	if stoppedOnPath(onPath, path, segment) {
		return ReasonStopped
	}
	return ReasonAborted
}

// lastResponse returns the last response entry of a run of entries, or
// nil.
func lastResponse(entries []Entry) *ResponseEntry {
	var last *ResponseEntry
	for _, e := range entries {
		if r, ok := e.(*ResponseEntry); ok {
			last = r
		}
	}
	return last
}

// madeCalls reports whether any of the calls is output of the response.
func madeCalls(calls []*Call, resp *ResponseEntry) bool {
	for _, c := range calls {
		if c.Entry.ResponseID == resp.ResponseID {
			return true
		}
	}
	return false
}

// stoppedOnPath is the format's stopped shape: the last response on
// the path before the segment's end has calls, every call on the path
// has an output, and the segment holds at least one output or
// decision, so the run finished what an earlier or its own model call
// asked for and the harness chose not to call the model again. No
// response follows, since the response is the last one on the path.
func stoppedOnPath(calls []*Call, path, segment []Entry) bool {
	last := lastResponse(path)
	if last == nil {
		return false
	}
	if !madeCalls(calls, last) {
		return false
	}
	for _, c := range calls {
		if c.Pending() {
			return false
		}
	}
	return answersACall(segment)
}

// answersACall reports whether a segment holds a function call output
// or a decision, which is what a run that answered a call rather than
// calling the model leaves behind.
func answersACall(segment []Entry) bool {
	for _, e := range segment {
		switch v := e.(type) {
		case *DecisionEntry:
			return true
		case *ItemEntry:
			if _, ok := v.Item.(*openresponses.FunctionCallOutput); ok {
				return true
			}
		}
	}
	return false
}

// ErrReasonMismatch is returned by [Run.Verify] when a run's written
// end reason or pending list disagrees with its segment.
var ErrReasonMismatch = errors.New("agentsession: run end disagrees with its segment")

// Verify checks the run's end entry against its segment. A written
// error or interrupted stands over any segment; any other reason must
// match [ComputeReason], and the pending list must match
// [Run.Pending]. A run without an end verifies
// trivially.
func (r *Run) Verify() error {
	if r.End == nil {
		return nil
	}
	if r.End.Reason != ReasonError && r.End.Reason != ReasonInterrupted {
		if got := ComputeReason(r.Path, r.Segment); got != r.End.Reason {
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

// ErrCallRejected is returned when a dispatch or a decision is
// appended for a call that a decision on the path already rejected:
// what follows a reject is the call's refusal output and nothing else.
var ErrCallRejected = errors.New("agentsession: call was rejected")

// ErrBadTarget is returned when a decision or dispatch is appended
// whose target is not the entry of a function call on the path with
// the decision's call ID: a target names the call, and its call ID
// must agree.
var ErrBadTarget = errors.New("agentsession: target does not name the call")

// ErrCallIDRepeated is returned when a function call is appended whose
// call ID another function call in the session has, on any branch, and
// by [Session.VerifyRecords] for a session that holds two: a call ID
// names one call in a session.
var ErrCallIDRepeated = errors.New("agentsession: call ID repeated")

// ErrCallIDEmpty is returned when a function call is appended with no
// call ID, and by [Session.VerifyRecords] for a session that holds one:
// nothing could name the call, not its output, a decision or a pending
// list.
var ErrCallIDEmpty = errors.New("agentsession: function call has no call ID")

// ErrSourceMismatch is returned by [Session.VerifyRecords] for a run
// start whose source is not the shape of its segment; see
// [ComputeSource].
var ErrSourceMismatch = errors.New("agentsession: run source disagrees with its segment")

// ErrRejectDispatched is returned when a reject is appended for a call
// that has a dispatch on the path: a reject says the call did not run,
// and one that may have run is ended by an answer.
var ErrRejectDispatched = errors.New("agentsession: reject for a call that was dispatched")

// ErrCallAnswered is returned when a dispatch or a decision is
// appended for a call that an answer decision on the path already
// ended: what follows an answer is the call's output and nothing else.
var ErrCallAnswered = errors.New("agentsession: call was answered")

// ErrCallCompleted is returned when an answer, a reject or a dispatch
// is appended for a call whose output is already on the path: the
// output ends the call, and an answer or reject is for a call that has
// none.
var ErrCallCompleted = errors.New("agentsession: call has its output")

// ErrAnswerNotDispatched is returned when an answer decision is
// appended for a call with no dispatch in a session whose header
// promises dispatch records: the record says that call never started,
// so it did not run, and a writer that ends it without running it
// writes a reject.
var ErrAnswerNotDispatched = errors.New("agentsession: answer for a call the record shows never started")

// ErrRecordMissing is returned by [Session.VerifyRecords] when the
// header promises a record entry type and the path lacks one where the
// event plainly happened.
var ErrRecordMissing = errors.New("agentsession: promised record entry missing")

// VerifyRecords checks the record entries on the path to leaf against
// the format's rules: every function call has a call ID and none
// repeats in the session, every decision and dispatch names its call
// by target, every run start's source and every run end agree with
// the segment, no dispatch or decision follows a reject or an answer
// on the same call, no answer, reject or dispatch follows an output and
// no reject a dispatch, and, when the header names dispatch in records,
// no answer ends a call with no dispatch and every call that ran has a
// dispatch. The rules that rest on records apply to what the session
// wrote, the entries after its base: a fork's prefix is another
// session's record, kept to that session's promise. It returns the
// first problem found.
func (s *Session) VerifyRecords(leaf string) error {
	path := s.Path(leaf)
	if path == nil {
		return fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	// Call identity is checked first, since every other rule reads the
	// calls by it: a call ID names one call in the session, on any
	// branch, and a decision or dispatch names by target a function
	// call before it on the path with its call ID.
	if err := s.verifyCallIDs(); err != nil {
		return err
	}
	if err := verifyTargets(path); err != nil {
		return err
	}
	calls := Calls(path)
	for _, r := range Runs(path) {
		if err := r.Verify(); err != nil {
			return err
		}
		if src := r.Start.Source; (src == SourceInput || src == SourceResume) && src != ComputeSource(r) {
			return fmt.Errorf("%w: run %s starts as %s and its segment is the shape of %s", ErrSourceMismatch, r.RunID(), src, ComputeSource(r))
		}
	}
	h := s.Header()
	// promised says the header's records promise covers the entry: one
	// the session wrote, after its base. An answer is owed a dispatch
	// when its call was made here, since one made in a prefix that
	// promised nothing may have run unrecorded, and the answer says the
	// outcome is unknown; an output is owed one when it was written
	// here, since this writer ran the tool, unless an answer stood for
	// the run.
	promised := func(id string) bool { return h.HasRecord(TypeDispatch) && !s.Prefix(id) }
	for _, c := range calls {
		if c.ended != nil {
			if c.ended.Verdict == VerdictAnswer {
				return fmt.Errorf("%w: dispatch %s follows an answer to call %s", ErrCallAnswered, c.endedBy.ID, c.ID())
			}
			return fmt.Errorf("%w: dispatch %s follows a reject of call %s", ErrCallRejected, c.endedBy.ID, c.ID())
		}
		if c.afterAnswer != nil {
			return fmt.Errorf("%w: decision %s follows an answer to call %s", ErrCallAnswered, c.afterAnswer.ID, c.ID())
		}
		if c.afterReject != nil {
			return fmt.Errorf("%w: decision %s follows a reject of call %s", ErrCallRejected, c.afterReject.ID, c.ID())
		}
		if c.rejectAfterDispatch != nil {
			return fmt.Errorf("%w: reject %s follows a dispatch of call %s", ErrRejectDispatched, c.rejectAfterDispatch.ID, c.ID())
		}
		if c.dispatchAfterOutput != nil {
			return fmt.Errorf("%w: dispatch %s follows the output of call %s", ErrCallCompleted, c.dispatchAfterOutput.ID, c.ID())
		}
		if c.endAfterOutput != nil {
			return fmt.Errorf("%w: %s %s follows the output of call %s", ErrCallCompleted, c.endAfterOutput.Verdict, c.endAfterOutput.ID, c.ID())
		}
		if c.Answered() && c.Dispatch == nil && promised(c.Entry.ID) {
			return fmt.Errorf("%w: call %s", ErrAnswerNotDispatched, c.ID())
		}
		if c.Output != nil && c.Dispatch == nil && !c.Rejected() && !c.Answered() && promised(c.Output.ID) {
			return fmt.Errorf("%w: call %s has an output and no dispatch", ErrRecordMissing, c.ID())
		}
	}
	return nil
}

// verifyCallIDs reports a call ID two function calls in the session
// share, on any branch.
func (s *Session) verifyCallIDs() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]string{}
	for _, e := range s.entries {
		it, ok := e.(*ItemEntry)
		if !ok {
			continue
		}
		if fc, ok := it.Item.(*openresponses.FunctionCall); ok {
			if fc.CallID == "" {
				return fmt.Errorf("%w: %s", ErrCallIDEmpty, it.ID)
			}
			if first, dup := seen[fc.CallID]; dup {
				return fmt.Errorf("%w: %s at %s and %s", ErrCallIDRepeated, fc.CallID, first, it.ID)
			}
			seen[fc.CallID] = it.ID
		}
	}
	return nil
}

// verifyTargets reports a decision or dispatch on the path whose target
// is not a function call before it on the path, or names one with
// another call ID.
func verifyTargets(path []Entry) error {
	calls := map[string]string{}
	check := func(e Entry, callID, target string) error {
		got, ok := calls[target]
		if !ok {
			return fmt.Errorf("%w: %s %s names %s, no function call before it on the path", ErrBadTarget, e.EntryType(), e.Base().ID, target)
		}
		if got != callID {
			return fmt.Errorf("%w: %s %s for call %s names the entry of call %s", ErrBadTarget, e.EntryType(), e.Base().ID, callID, got)
		}
		return nil
	}
	for _, e := range path {
		switch v := e.(type) {
		case *ItemEntry:
			if fc, ok := v.Item.(*openresponses.FunctionCall); ok {
				calls[v.ID] = fc.CallID
			}
		case *DecisionEntry:
			if err := check(v, v.CallID, v.Target); err != nil {
				return err
			}
		case *DispatchEntry:
			if err := check(v, v.CallID, v.Target); err != nil {
				return err
			}
		}
	}
	return nil
}
