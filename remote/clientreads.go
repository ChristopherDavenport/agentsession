package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/url"
	"strconv"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// LogRecord is one record of a log read: an append, with the entry it
// appended, or a head move, with Entry empty.
type LogRecord struct {
	// Seq is the entry's place among the session's entries, from 1; for a
	// head move, the number of entries before it.
	Seq int
	// Entry is the ID of the entry the record appended, "" for a head
	// move.
	Entry string
	// Head is the session's head after the record, "" for none.
	Head string
	// Outcome is what the append did: continued, branched, leaf_moved or
	// leaf_not_moved; "" for a head move.
	Outcome string
	// Durable is set when the record was committed when it was read.
	Durable bool
	// Cursor is the position after the record, to read or follow from.
	Cursor agentsession.Cursor
}

// Log is a session's log as one log read answered it.
type Log struct {
	Header      agentsession.Header
	Incarnation string
	// Mark is the record mark where the server's store keeps one.
	Mark string
	// Reset is set when the cursor asked for named no place in the log:
	// Records are then the whole log.
	Reset   bool
	Records []LogRecord
	// Head is the session's head at the read's end, "" for none.
	Head string
	// Cursor is where the read ended.
	Cursor agentsession.Cursor
}

// Log reads the session's log after the cursor after, from its start
// when after is empty: RFC 0003's log read. A cursor is a log read's or
// a follow's. With durable it reads only what is committed, which the
// server commits first where it holds the session's writer. It is
// answered by a server whose store keeps a log of records (the
// capability `log`); durable needs `committed_view`.
func (c *Client) Log(ctx context.Context, id string, after agentsession.Cursor, durable bool) (Log, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", string(after))
	}
	if durable {
		q.Set("durable", "true")
	}
	var out Log
	var end json.RawMessage
	first := true
	for data, err := range c.linesTo(ctx, "/sessions/"+url.PathEscape(id)+"/log", q, &end) {
		if err != nil {
			return Log{}, err
		}
		var l logLine
		if err := json.Unmarshal(data, &l); err != nil {
			return Log{}, fmt.Errorf("remote: log of %s: %w", id, err)
		}
		if first {
			first = false
			if err := out.state(l.Header, l.Incarnation, l.Mark, l.Reset); err != nil {
				return Log{}, fmt.Errorf("remote: log of %s: %w", id, err)
			}
		}
		out.Records = append(out.Records, LogRecord{Seq: l.Seq, Entry: l.Entry, Head: headIn(l.Head), Outcome: l.Outcome, Durable: l.Durable, Cursor: agentsession.Cursor(l.Cursor)})
	}
	var e struct {
		Cursor      string          `json:"cursor"`
		Head        *string         `json:"head"`
		Header      json.RawMessage `json:"header"`
		Incarnation string          `json:"incarnation"`
		Mark        string          `json:"mark"`
		Reset       bool            `json:"reset"`
	}
	if err := json.Unmarshal(end, &e); err != nil {
		return Log{}, fmt.Errorf("remote: log of %s: the end frame: %w", id, err)
	}
	if first {
		if err := out.state(e.Header, e.Incarnation, e.Mark, e.Reset); err != nil {
			return Log{}, fmt.Errorf("remote: log of %s: %w", id, err)
		}
	}
	out.Head, out.Cursor = headIn(e.Head), agentsession.Cursor(e.Cursor)
	return out, nil
}

// state sets what the first line, or the end frame of an empty read,
// says of the session.
func (l *Log) state(header json.RawMessage, inc, mark string, reset bool) error {
	if len(header) > 0 {
		if err := json.Unmarshal(header, &l.Header); err != nil {
			return err
		}
	}
	l.Incarnation, l.Mark, l.Reset = inc, mark, reset
	return nil
}

// PathEntry is one entry of a path read: its envelope always, its
// content and the decoded entry when the read carried the content.
//
// Entry is decoded from its line alone, as [agentsession.UnmarshalEntry]
// decodes one: a path read does not carry the session's header, so
// Entry is read as this package writes it. In a session whose header
// names a minor before 0.13, an entry's redacted member is one that
// minor does not define; a caller that needs it read so takes the
// format from [Client.Session] or a log read's header and applies
// [agentsession.GateEntry].
type PathEntry struct {
	ID       string
	Envelope json.RawMessage
	// Content is nil for an entry whose content the read left out.
	Content json.RawMessage
	// Entry is the entry decoded, nil when Content is.
	Entry agentsession.Entry
}

// Path is a session's path as one path read answered it, root first.
// A shallow path (Contents "since-fold") is not the session: it lacks
// the contents the fold replaced, which Lacks names, and is no RFC 0001
// file; what it holds rebuilds the request at its leaf.
type Path struct {
	Head        string
	Cursor      agentsession.Cursor
	Incarnation string
	// Contents is what the server answered: all, since-fold or none. A
	// since-fold read the server could not make shallow comes back all.
	Contents string
	Entries  []PathEntry
}

// Lacks names the entries whose content the path does not carry.
func (p Path) Lacks() []string {
	var out []string
	for _, e := range p.Entries {
		if e.Content == nil {
			out = append(out, e.ID)
		}
	}
	return out
}

// Path reads the path from the session's root to leaf (the head when
// leaf is empty), root first, with contents "all", "since-fold" or
// "none" ("all" when empty): RFC 0003's path read. Every envelope is
// verified by its hash and its parent link, and every content carried
// by the hash its envelope names, before Path returns, so the chain
// holds from the response alone.
func (c *Client) Path(ctx context.Context, id, leaf, contents string) (Path, error) {
	q := url.Values{}
	if leaf != "" {
		q.Set("leaf", leaf)
	}
	if contents != "" {
		q.Set("contents", contents)
	}
	var out Path
	var end json.RawMessage
	first := true
	for data, err := range c.linesTo(ctx, "/sessions/"+url.PathEscape(id)+"/path", q, &end) {
		if err != nil {
			return Path{}, err
		}
		var l pathLine
		if err := json.Unmarshal(data, &l); err != nil {
			return Path{}, fmt.Errorf("remote: path of %s: %w", id, err)
		}
		if first {
			first = false
			out.Head, out.Cursor, out.Incarnation, out.Contents = headIn(l.Head), agentsession.Cursor(l.Cursor), l.Incarnation, l.Contents
		}
		pe, err := pathEntryOf(l)
		if err != nil {
			return Path{}, fmt.Errorf("remote: path of %s: %w", id, err)
		}
		parent, err := envelopeParent(l.Envelope)
		if err != nil {
			return Path{}, fmt.Errorf("remote: path of %s: entry %s: %w", id, l.Entry, err)
		}
		want := ""
		if n := len(out.Entries); n > 0 {
			want = out.Entries[n-1].ID
		}
		if parent != want {
			return Path{}, fmt.Errorf("remote: path of %s: entry %s hangs from %q, not the entry before it %q", id, l.Entry, parent, want)
		}
		out.Entries = append(out.Entries, pe)
	}
	if first {
		var e pathLine
		if err := json.Unmarshal(end, &e); err != nil {
			return Path{}, fmt.Errorf("remote: path of %s: the end frame: %w", id, err)
		}
		out.Head, out.Cursor, out.Incarnation, out.Contents = headIn(e.Head), agentsession.Cursor(e.Cursor), e.Incarnation, e.Contents
	}
	return out, nil
}

// pathEntryOf verifies a path line and decodes its entry when it
// carries the content.
func pathEntryOf(l pathLine) (PathEntry, error) {
	id, err := agentsession.HashRequestJSON(l.Envelope)
	if err != nil {
		return PathEntry{}, err
	}
	if id != l.Entry {
		return PathEntry{}, fmt.Errorf("%w: the envelope of %s hashes to %s", agentsession.ErrBadID, l.Entry, id)
	}
	pe := PathEntry{ID: l.Entry, Envelope: l.Envelope}
	if len(l.Content) == 0 || bytes.Equal(bytes.TrimSpace(l.Content), []byte("null")) {
		return pe, nil
	}
	want, ok := cas.ContentOf(l.Envelope)
	if !ok {
		return PathEntry{}, fmt.Errorf("the envelope of %s names no content", l.Entry)
	}
	got, err := agentsession.HashRequestJSON(l.Content)
	if err != nil {
		return PathEntry{}, err
	}
	if got != want {
		return PathEntry{}, fmt.Errorf("%w: the content of %s hashes to %s, not %s", agentsession.ErrBadID, l.Entry, got, want)
	}
	line, err := joinLine(l.Entry, l.Envelope, l.Content)
	if err != nil {
		return PathEntry{}, err
	}
	e, err := agentsession.UnmarshalEntry(line)
	if err != nil {
		return PathEntry{}, fmt.Errorf("entry %s: %w", l.Entry, err)
	}
	pe.Content, pe.Entry = l.Content, e
	return pe, nil
}

// joinLine rebuilds an entry's RFC 0001 line from its envelope and its
// content, as the wire carries them, and checks it hashes to id.
func joinLine(id string, envelope, content json.RawMessage) ([]byte, error) {
	var env, body map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, fmt.Errorf("entry %s: envelope: %w", id, err)
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return nil, fmt.Errorf("entry %s: content: %w", id, err)
	}
	line := map[string]json.RawMessage{"id": json.RawMessage(strconv.Quote(id))}
	for k, v := range env {
		if k != "content" {
			line[k] = v
		}
	}
	for k, v := range body {
		line[k] = v
	}
	data, err := json.Marshal(line)
	if err != nil {
		return nil, err
	}
	got, _, err := agentsession.EntryHashes(data)
	if err != nil {
		return nil, err
	}
	if got != id {
		return nil, fmt.Errorf("%w: entry %s rebuilds to %s", agentsession.ErrBadID, id, got)
	}
	return data, nil
}

// envelopeParent is the parent an envelope names, "" for none.
func envelopeParent(envelope json.RawMessage) (string, error) {
	var e struct {
		Parent *string `json:"parent"`
	}
	if err := json.Unmarshal(envelope, &e); err != nil {
		return "", err
	}
	if e.Parent == nil {
		return "", nil
	}
	return *e.Parent, nil
}

// FollowCommitted follows the session as [Client.Follow] does, with
// only what is committed: RFC 0003's durable=true. The server commits
// the session where it holds its writer before it sends, so what is
// yielded survives a crash of the server's store. It needs the
// capability `committed_view`.
func (c *Client) FollowCommitted(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return c.follow(ctx, id, from, true)
}

// RefState is a followed ref's target, and whether its session is gone
// or another now holds its ID.
type RefState struct {
	Name     string
	Target   agentsession.RefTarget
	Dangling bool
}

// FollowRef follows a ref: its target on connect, and again each time
// the target or its state changes, until ctx is done. A ref deleted is
// a zero target. It needs the capability `ref_follow`. A stream cut
// ends with [ErrCut]; a reconnect is sent the current state again, as
// a ref has no cursor.
func (c *Client) FollowRef(ctx context.Context, name string) iter.Seq2[RefState, error] {
	return func(yield func(RefState, error) bool) {
		if err := agentsession.ValidRefName(name); err != nil {
			yield(RefState{}, err)
			return
		}
		resp, err := c.get(ctx, "/refs/"+name, url.Values{"view": {"follow"}}, nil)
		if err != nil {
			yield(RefState{}, err)
			return
		}
		defer resp.Body.Close()
		for ev, err := range readSSE(resp.Body) {
			if err != nil {
				if ctx.Err() == nil {
					yield(RefState{}, fmt.Errorf("%w: follow of ref %s: %v", ErrCut, name, err))
				}
				return
			}
			switch ev.name {
			case eventTarget:
				var s refState
				if err := json.Unmarshal(ev.data, &s); err != nil {
					yield(RefState{}, fmt.Errorf("remote: follow of ref %s: %w", name, err))
					return
				}
				if !yield(RefState{Name: s.Name, Target: s.Target.lib(), Dangling: s.State == "dangling"}, nil) {
					return
				}
			case eventError:
				p := &Problem{}
				if err := json.Unmarshal(ev.data, p); err != nil || p.Code == "" {
					yield(RefState{}, fmt.Errorf("remote: follow of ref %s: an error event that does not parse", name))
					return
				}
				yield(RefState{}, p)
				return
			}
		}
	}
}

// fill reads the lines of a snapshot's records a server sent without
// them, past its snapshot bound, as objects: an envelope and its
// content, checked against the record's entry.
func (c *Client) fill(ctx context.Context, d *snapshotData) error {
	for i, r := range d.Records {
		if len(r.Line) > 0 {
			continue
		}
		env, err := c.Object(ctx, cas.SpaceEntries, r.Entry)
		if err != nil {
			return fmt.Errorf("remote: snapshot record %d: %w", r.Seq, err)
		}
		ch, ok := cas.ContentOf(env)
		if !ok {
			return fmt.Errorf("remote: snapshot record %d: the envelope of %s names no content", r.Seq, r.Entry)
		}
		body, err := c.Object(ctx, cas.SpaceContents, ch)
		if err != nil {
			return fmt.Errorf("remote: snapshot record %d: %w", r.Seq, err)
		}
		line, err := cas.JoinLine(r.Entry, env, body)
		if err != nil {
			return fmt.Errorf("remote: snapshot record %d: %w", r.Seq, err)
		}
		d.Records[i].Line = line
	}
	return nil
}

// Fetch takes a session from the server into a local cas store, as
// cas.Store.Fetch takes one from another store: it reads the server's
// committed log (a durable read, so nothing a crash of the server could
// take back), the entries the log names and the media their contents
// name as objects, and a fork's prefix as its path, and admits them as
// a fetch. A session the local store lacked is created as a mirror; the
// head moves on a mirror only to a head that descends from its own.
func Fetch(ctx context.Context, local *cas.Store, c *Client, id string) (cas.Exchange, error) {
	x, _, err := fetch(ctx, local, c, id)
	return x, err
}

// fetch is Fetch, returning the head it fetched as well.
func fetch(ctx context.Context, local *cas.Store, c *Client, id string) (cas.Exchange, string, error) {
	lg, err := c.Log(ctx, id, "", true)
	if err != nil {
		return cas.Exchange{}, "", err
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, agentsession.New(lg.Header)); err != nil {
		return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: the header: %w", id, err)
	}
	var prefix, own []string
	if base := lg.Header.Base; base != "" {
		p, err := c.Path(ctx, id, base, "all")
		if err != nil {
			return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: its prefix: %w", id, err)
		}
		for _, e := range p.Entries {
			line, err := joinLine(e.ID, e.Envelope, e.Content)
			if err != nil {
				return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: its prefix: %w", id, err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
			prefix = append(prefix, e.ID)
		}
	}
	blobs := map[string][]byte{}
	seen := map[string]bool{}
	for _, r := range lg.Records {
		if r.Entry == "" || seen[r.Entry] {
			continue
		}
		seen[r.Entry] = true
		env, err := c.Object(ctx, cas.SpaceEntries, r.Entry)
		if err != nil {
			return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: %w", id, err)
		}
		ch, ok := cas.ContentOf(env)
		if !ok {
			return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: the envelope of %s names no content", id, r.Entry)
		}
		body, err := c.Object(ctx, cas.SpaceContents, ch)
		if err != nil {
			return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: %w", id, err)
		}
		for _, h := range cas.BlobsNamedBy(body) {
			if _, ok := blobs[h]; ok {
				continue
			}
			if blobs[h], err = c.Object(ctx, cas.SpaceMedia, h); err != nil {
				return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: %w", id, err)
			}
		}
		line, err := cas.JoinLine(r.Entry, env, body)
		if err != nil {
			return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: %w", id, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
		own = append(own, r.Entry)
	}
	// The closure is read as a session file is, so every entry is hashed
	// and decoded by the format's own reader before the store admits it.
	sess, err := agentsession.Read(&buf)
	if err != nil {
		return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: %w", id, err)
	}
	if sess.Truncated() != nil {
		return cas.Exchange{}, "", fmt.Errorf("remote: fetch %s: a line does not parse", id)
	}
	pick := func(ids []string) ([]agentsession.Entry, error) {
		out := make([]agentsession.Entry, 0, len(ids))
		for _, eid := range ids {
			e, ok := sess.Entry(eid)
			if !ok {
				return nil, fmt.Errorf("remote: fetch %s: entry %s is not in what was read", id, eid)
			}
			out = append(out, e)
		}
		return out, nil
	}
	cl := cas.Closure{Header: lg.Header, Mark: lg.Mark, Head: lg.Head, Blobs: blobs}
	if cl.Prefix, err = pick(prefix); err != nil {
		return cas.Exchange{}, "", err
	}
	if cl.Own, err = pick(own); err != nil {
		return cas.Exchange{}, "", err
	}
	x, err := local.Receive(ctx, cl, cas.ReceiveOptions{Fetch: true})
	return x, lg.Head, err
}

// TakeBack makes a local cas store the record of a session the server
// is the record for: it fetches the session, hands the server's record
// over at the head it fetched, which leaves the server a mirror and is
// refused with [ErrHeadMoved] if a writer moved the head since (fetch
// and try again), and declares the local store the record at that head.
// The handover takes and ends a lease, so no writer at the server goes
// on after it. A failure after the handover leaves no record; the
// local store holds everything, and DeclareRecordAt on it finishes the
// move.
func TakeBack(ctx context.Context, local *cas.Store, c *Client, id string) (cas.Exchange, error) {
	x, head, err := fetch(ctx, local, c, id)
	if err != nil {
		return x, err
	}
	if err := c.HandOver(ctx, id, head); err != nil {
		return x, err
	}
	if err := local.DeclareRecordAt(ctx, id, head); err != nil {
		return x, fmt.Errorf("remote: take back %s: the server handed it over; the local store could not declare the record: %w", id, err)
	}
	x.Handover = true
	return x, nil
}
