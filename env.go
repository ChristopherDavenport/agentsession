package agentsession

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/openresponses"
)

// HashFile returns the content hash of a file in the form the env entry
// uses: "sha256:" followed by lowercase hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("agentsession: hash %s: %w", path, err)
	}
	return HashPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes returns the content hash of data in the env entry's form.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return HashPrefix + hex.EncodeToString(sum[:])
}

// HashFiles hashes each path, keyed as given. Paths are hashed as they
// are; callers that want keys relative to a directory pass them that
// way and resolve against dir.
func HashFiles(dir string, paths ...string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		full := p
		if dir != "" && !filepath.IsAbs(p) {
			full = filepath.Join(dir, p)
		}
		h, err := HashFile(full)
		if err != nil {
			return nil, err
		}
		out[p] = h
	}
	return out, nil
}

// NewEnvEntry starts an env entry for a working directory. Use
// [EnvEntry.SetGit], [EnvEntry.AddRead] and [EnvEntry.AddWritten] to
// fill it in.
func NewEnvEntry(cwd string) *EnvEntry {
	return &EnvEntry{CWD: cwd}
}

// SetGit records the git revision of dir, read from its .git directory
// without running git, and whether the tree is known to be dirty. A
// directory that is not a git checkout leaves VCS unset and returns
// nil; other errors are returned. Members of an existing VCS the
// format does not define, set with [VCS.SetMember] or read from a
// file, are kept.
func (e *EnvEntry) SetGit(dir string, dirty bool) error {
	rev, err := GitRevision(dir)
	if err != nil {
		if errors.Is(err, ErrNotGit) {
			return nil
		}
		return err
	}
	v := &VCS{System: "git", Revision: rev, Dirty: dirty}
	if e.VCS != nil {
		v.Unknown = e.VCS.Unknown
	}
	e.VCS = v
	return nil
}

// AddRead records the hash of a file the session read.
func (e *EnvEntry) AddRead(path, hash string) {
	if e.Files == nil {
		e.Files = &FileHashes{}
	}
	if e.Files.Read == nil {
		e.Files.Read = map[string]string{}
	}
	e.Files.Read[path] = hash
}

// AddWritten records the hash of a file the session wrote.
func (e *EnvEntry) AddWritten(path, hash string) {
	if e.Files == nil {
		e.Files = &FileHashes{}
	}
	if e.Files.Written == nil {
		e.Files.Written = map[string]string{}
	}
	e.Files.Written[path] = hash
}

// SetWorkspace records which file system CWD is a path in: kind is
// [WorkspaceLocal], [WorkspaceContainer] or [WorkspaceRemote], and ref
// is what the harness resolves to it (an image digest, an endpoint),
// empty for a local workspace. The machine goes in the workspace's
// host member and a restarted file system's identity in its instance
// member, which the format defines since 0.14: set them, and anything
// else that tells one file system from another, with
// [Workspace.SetMember] on the result, and not as an unknown member of
// the entry, which the substitution rule does not compare.
func (e *EnvEntry) SetWorkspace(kind, ref string) *Workspace {
	e.Workspace = &Workspace{Kind: kind, Ref: ref}
	return e.Workspace
}

// AddTool records the version of a tool available to the session.
func (e *EnvEntry) AddTool(name, version string) {
	if e.Tools == nil {
		e.Tools = map[string]string{}
	}
	e.Tools[name] = version
}

// ErrNotGit is returned by [GitRevision] when dir is not inside a git
// checkout.
var ErrNotGit = errors.New("agentsession: not a git checkout")

// GitRevision returns the commit HEAD points at for the checkout
// containing dir, reading .git directly so no git binary is needed. It
// follows symbolic refs through loose refs and packed-refs and handles
// worktrees and submodules whose .git is a file.
func GitRevision(dir string) (string, error) {
	gitDir, err := findGitDir(dir)
	if err != nil {
		return "", err
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", fmt.Errorf("agentsession: read HEAD: %w", err)
	}
	ref := strings.TrimSpace(string(head))
	if !strings.HasPrefix(ref, "ref: ") {
		return ref, nil // detached
	}
	ref = strings.TrimPrefix(ref, "ref: ")
	// Refs of a worktree live in the common dir.
	common := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common = filepath.Join(gitDir, strings.TrimSpace(string(data)))
	}
	if data, err := os.ReadFile(filepath.Join(common, filepath.FromSlash(ref))); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	packed, err := os.Open(filepath.Join(common, "packed-refs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("agentsession: unborn ref %s", ref)
		}
		return "", err
	}
	defer packed.Close()
	sc := bufio.NewScanner(packed)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == ref {
			return sha, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("agentsession: unborn ref %s", ref)
}

// findGitDir walks up from dir to the nearest .git directory or file.
func findGitDir(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Stat(candidate)
		if err == nil {
			if info.IsDir() {
				return candidate, nil
			}
			data, err := os.ReadFile(candidate)
			if err != nil {
				return "", err
			}
			target := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return target, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotGit
		}
		dir = parent
	}
}

// NewOutcomeEntry builds an outcome entry of the given kind about
// target, which may be "" for the session as a whole.
func NewOutcomeEntry(kind, target string) *OutcomeEntry {
	return &OutcomeEntry{Kind: kind, Target: target}
}

// WithScore sets the score and returns the entry, for chaining.
func (e *OutcomeEntry) WithScore(score float64) *OutcomeEntry {
	e.Score = &score
	return e
}

// WithPass sets the judge's verdict and returns the entry, for
// chaining.
func (e *OutcomeEntry) WithPass(pass bool) *OutcomeEntry {
	e.Pass = &pass
	return e
}

// WithLabel sets the label and returns the entry, for chaining.
func (e *OutcomeEntry) WithLabel(label string) *OutcomeEntry {
	e.Label = label
	return e
}

// WithDetails encodes v as the details and returns the entry, for
// chaining. An encoding failure leaves Details unset and is returned by
// the next Session.Append through MarshalEntry only if v cannot be
// marshalled at all, so callers should pass plain data.
func (e *OutcomeEntry) WithDetails(v any) *OutcomeEntry {
	if data, err := json.Marshal(v); err == nil {
		e.Details = data
	}
	return e
}

// NewLinkEntry builds a link to another session.
func NewLinkEntry(rel, session string) *LinkEntry {
	return &LinkEntry{Rel: rel, Session: session}
}

// NewSubsessionLink builds the link entry the format prescribes for a
// subagent session spawned by the function call callID.
func NewSubsessionLink(session, callID string) *LinkEntry {
	return &LinkEntry{Rel: RelSubsession, Session: session, CallID: callID}
}

// NewJudgedByLink builds the link entry a judged session records for
// the session of its judge: judge is the judge's session ID, and target,
// when it is not empty, the entry of the judged session the judgement is
// about, the one the judge's outcome names. The judge is a session of
// its own whose header names the judged one as its parent_session, which
// a subagent's and a fork's header does too, so the link is what says
// which it is. [Judges] reads these links back.
func NewJudgedByLink(judge, target string) *LinkEntry {
	return &LinkEntry{Rel: RelJudgedBy, Session: judge, Target: target}
}

// Callee names a callee in another system, reached over a protocol
// such as A2A, for a [RelSubsession] or [RelPeer] link: the endpoint
// the call was sent to as the caller resolved it (never a name the
// callee claims for itself), the callee's task (empty when it answered
// without one, as an A2A message does) and conversation IDs, and its
// own session ID when it publishes one.
type Callee struct {
	URL     string
	Task    string
	Context string
	Session string
}

// NewPeerLink builds the link a caller records for a callee whose work
// outlives the call callID: the caller does not end it when it gives
// the call up, and the callee keeps its own record. It is written as
// soon as the caller learns the callee's task, or its answer when it
// gives none. From it a reader may conclude that the call's output came
// from that work, and not that the work can be replayed or that it
// ended with the call. A peer link names its call and a url or a
// session, which [Session.Append] and [Session.Prepare] hold a new one
// to.
func NewPeerLink(callID string, c Callee) *LinkEntry {
	return &LinkEntry{Rel: RelPeer, Session: c.Session, CallID: callID, URL: c.URL, Task: c.Task, Context: c.Context}
}

// NewCalleeSubsessionLink builds the subsession link for a callee in
// another system that the call callID started, whose work the caller
// ends, or asks to stop, when it gives the call up: the relation
// follows the callee's lifetime, not where it runs. It is written as
// soon as the caller learns the callee's task, after the call's
// dispatch; a call dispatched again starts another task and gets a
// link of its own. [Session.VerifyLinks] does not check its session's
// header, which was not derived from this one.
func NewCalleeSubsessionLink(callID string, c Callee) *LinkEntry {
	return &LinkEntry{Rel: RelSubsession, Session: c.Session, CallID: callID, URL: c.URL, Task: c.Task, Context: c.Context}
}

// NewCancelSentLink builds the notice a caller records when it tells
// the callee link names that it no longer wants the work: a link of the
// same relation and call, carrying [CancelSent], whose Target is link's
// entry ID, so link must have been appended, on the path the notice is
// appended to. It records that the caller asked, not that the work
// stopped; the caller does not wait for the callee's answer. It names
// no new callee, and [Callees] reads it as a notice about link. Its
// target names an entry of this session: whoever copies or re-points
// links, onto a new branch or into another store, re-points the notice
// with them.
func NewCancelSentLink(link *LinkEntry) *LinkEntry {
	return &LinkEntry{Rel: link.Rel, Session: link.Session, CallID: link.CallID, Target: link.ID, Cancel: CancelSent}
}

// CalleeLink is one callee of a call as a path records it: a
// subsession or peer link, the hand-off it belongs to, and what became
// of it.
type CalleeLink struct {
	*LinkEntry
	// Dispatch is the entry ID of the dispatch the link belongs to: for
	// a callee in another system (URL set), the latest dispatch of its
	// call before it on the path, since the callee names its work only
	// when it answers; for a subsession of this store, written before
	// the hand-off it starts, the first dispatch of its call after it.
	// It is empty when the path records no such dispatch.
	Dispatch string
	// Cancelled is true when a cancel notice on the path names this
	// link: the caller asked the callee to stop. It does not say the
	// work stopped.
	Cancelled bool
	// Answered is true when the call's output on the path came from
	// this callee: the output follows the link, the link belongs to the
	// call's last dispatch before the output, no cancel notice names
	// it, and no answer decision of the call comes between the link
	// and the output, which would make the output the harness's.
	Answered bool
}

// Callees returns the callees of the calls on a root-first path, every
// subsession and peer link in path order. A link is identified by its
// entry and the dispatch it belongs to, not by its task, which a
// callee that answered with a message has none of: a call dispatched
// more than once, to a callee that starts new work each time, has a
// link for each hand-off that reached one. A cancel notice is not a
// callee; it marks the link its target names, and one naming no link
// on the path is passed over.
func Callees(path []Entry) []CalleeLink {
	var out []CalleeLink
	byID := make(map[string]int)
	last := make(map[string]string)  // call -> latest dispatch so far
	before := make(map[string][]int) // call -> in-store links awaiting their dispatch
	open := make(map[string][]int)   // call -> links not yet followed by an output
	blocked := make(map[int]bool)    // link index -> an answer decision came after it
	for _, e := range path {
		switch v := e.(type) {
		case *DispatchEntry:
			last[v.CallID] = v.ID
			for _, i := range before[v.CallID] {
				out[i].Dispatch = v.ID
			}
			delete(before, v.CallID)
		case *DecisionEntry:
			if v.Verdict == VerdictAnswer {
				for _, i := range open[v.CallID] {
					blocked[i] = true
				}
			}
		case *LinkEntry:
			if !v.callee() {
				continue
			}
			if v.Cancel != "" {
				if i, ok := byID[v.Target]; ok {
					out[i].Cancelled = true
				}
				continue
			}
			i := len(out)
			byID[v.ID] = i
			c := CalleeLink{LinkEntry: v}
			if v.URL != "" {
				c.Dispatch = last[v.CallID]
			} else {
				before[v.CallID] = append(before[v.CallID], i)
			}
			out = append(out, c)
			open[v.CallID] = append(open[v.CallID], i)
		case *ItemEntry:
			o, ok := v.Item.(*openresponses.FunctionCallOutput)
			if !ok {
				continue
			}
			links := open[o.CallID]
			delete(open, o.CallID)
			for j := len(links) - 1; j >= 0; j-- {
				i := links[j]
				if blocked[i] || out[i].Dispatch != last[o.CallID] {
					continue
				}
				out[i].Answered = true
				break
			}
		}
	}
	// A cancel notice may come after the output it would have
	// answered; a cancelled callee did not answer.
	for i := range out {
		if out[i].Cancelled {
			out[i].Answered = false
		}
	}
	return out
}

// Callees returns the callees on the path to leaf; see [Callees].
func (s *Session) Callees(leaf string) ([]CalleeLink, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	return Callees(path), nil
}

// Judges returns the judged_by links on a root-first path, in path
// order: the sessions that judged what the path holds, each with the
// entry it judged in its Target when the writer named one. A link is a
// record entry and contributes nothing to a context, so a reader
// selecting the judges of a run reads them here and filters by Target
// for the judgements of one entry; the header of a judge's session
// cannot say it is one, since a subagent and a fork carry the same
// parent_session.
func Judges(path []Entry) []*LinkEntry {
	var out []*LinkEntry
	for _, e := range path {
		if l, ok := e.(*LinkEntry); ok && l.Rel == RelJudgedBy {
			out = append(out, l)
		}
	}
	return out
}

// Judges returns the judged_by links on the path to leaf; see [Judges].
func (s *Session) Judges(leaf string) ([]*LinkEntry, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, leaf)
	}
	return Judges(path), nil
}

// NewLabelEntry builds a label on target; an empty label clears.
func NewLabelEntry(target, label string) *LabelEntry {
	e := &LabelEntry{Target: target}
	if label != "" {
		e.Label = &label
	}
	return e
}
