package remote

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ChristopherDavenport/agentsession"
)

// replica is a session built from follow events, as a client keeps it
// and as the server keeps the copy it serves snapshots from. Every entry
// it takes is verified by its hash first.
type replica struct {
	sess *agentsession.Session
	// seq is the number of records the log holds.
	seq int
}

// newReplica reads the session a snapshot or reset carries.
func newReplica(d *snapshotData) (*replica, error) {
	var buf bytes.Buffer
	buf.Write(bytes.TrimSpace(d.Header))
	buf.WriteByte('\n')
	for i, r := range d.Records {
		if r.Seq != i+1 {
			return nil, fmt.Errorf("remote: snapshot record %d has seq %d", i+1, r.Seq)
		}
		if err := verifyLine(r.Line, r.Entry); err != nil {
			return nil, err
		}
		buf.Write(bytes.TrimSpace(r.Line))
		buf.WriteByte('\n')
	}
	sess, err := agentsession.Read(&buf)
	if err != nil {
		return nil, fmt.Errorf("remote: snapshot: %w", err)
	}
	if sess.Truncated() != nil {
		return nil, fmt.Errorf("remote: snapshot: a line does not parse")
	}
	if d.Session != "" && sess.ID() != d.Session {
		return nil, fmt.Errorf("remote: snapshot of %s carries session %s", d.Session, sess.ID())
	}
	r := &replica{sess: sess, seq: len(d.Records)}
	if err := r.place(headIn(d.Head)); err != nil {
		return nil, err
	}
	return r, nil
}

// appended takes the entry an appended event carries and puts the head
// where the event says it is after it. It returns the entry, the leaf
// before it and the leaf after.
func (r *replica) appended(d *appendedData) (e agentsession.Entry, before string, err error) {
	if d.Seq != r.seq+1 {
		return nil, "", fmt.Errorf("remote: appended record has seq %d after %d", d.Seq, r.seq)
	}
	if err := verifyLine(d.Line, d.Entry); err != nil {
		return nil, "", err
	}
	e, err = agentsession.UnmarshalEntry(d.Line)
	if err != nil {
		return nil, "", fmt.Errorf("remote: appended %s: %w", d.Entry, err)
	}
	before = r.sess.Leaf()
	res, err := r.sess.Extend(e)
	if err != nil {
		return nil, "", fmt.Errorf("remote: appended %s: %w", d.Entry, err)
	}
	if res.ID != d.Entry {
		return nil, "", fmt.Errorf("remote: appended %s: %w: it reads as %s", d.Entry, agentsession.ErrBadID, res.ID)
	}
	if res.Outcome == agentsession.Held {
		return nil, "", fmt.Errorf("remote: appended %s, which the session already held", d.Entry)
	}
	r.seq++
	if err := r.place(headIn(d.Head)); err != nil {
		return nil, "", err
	}
	return e, before, nil
}

// place puts the leaf where the server says it is. The server's store
// may place a leaf by a rule the reader's would not (a cas writer's live
// rule), and the head it reports is the record's.
func (r *replica) place(head string) error {
	if r.sess.Leaf() == head {
		return nil
	}
	if head == "" {
		r.sess.ResetLeaf()
		return nil
	}
	if err := r.sess.Branch(head); err != nil {
		return fmt.Errorf("remote: head %s: %w", head, err)
	}
	return nil
}

// verifyLine checks an entry's line hashes to the ID it carries and to
// want, the ID the record names.
func verifyLine(line json.RawMessage, want string) error {
	id, _, err := agentsession.EntryHashes(line)
	if err != nil {
		return fmt.Errorf("remote: entry %s: %w", want, err)
	}
	var named struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &named); err != nil {
		return fmt.Errorf("remote: entry %s: %w", want, err)
	}
	if id != named.ID || id != want {
		return fmt.Errorf("remote: entry %s: %w: its line hashes to %s and names %s", want, agentsession.ErrBadID, id, named.ID)
	}
	return nil
}

// snapshotOf encodes a session from the start, every entry inlined.
func snapshotOf(sess *agentsession.Session) (*snapshotData, error) {
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, sess); err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'})
	h := sess.Header()
	d := &snapshotData{
		Session:     h.ID,
		Header:      json.RawMessage(lines[0]),
		Incarnation: incarnation(h),
		Head:        headOut(sess.Leaf()),
		Records:     make([]record, 0, len(lines)-1),
	}
	for i, l := range lines[1:] {
		var named struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(l, &named); err != nil {
			return nil, err
		}
		d.Records = append(d.Records, record{Seq: i + 1, Entry: named.ID, Line: json.RawMessage(l)})
	}
	return d, nil
}
