// Package spanner is the content-addressed session store RFC 0002
// describes, over Cloud Spanner. It is a nested module so the Spanner
// client stays out of the library's dependency graph.
//
// The layout is the RFC's reference schema, in [DDL]:
//
//	Contents  a body or a media blob, canonical bytes, held once by hash
//	Entries   an envelope: type, parent, content hash, canonical bytes
//	Sidecars  which media blobs a content names through a sidecar: URL
//	Edges     parent to child, the reverse index hashes cannot give
//	Sessions  the ref: header, base, head, record mark, log length
//	Log       a session's own entries by sequence, interleaved in it
//	Prefix    the entries on a session's path to its base, interleaved
//
// An append is one read-write transaction: the objects, the log row and
// the head, all or none. The transaction is the commit point RFC 0002
// asks for, and Spanner's commit is replicated before it returns, so
// every acknowledged append is durable and no store-side journal is
// needed. Spanner's write-ahead log is the journal.
//
// No session is held by a process. Several processes append to one
// session at once, as the RFC's concurrency model allows: each append
// reads the session row inside its transaction, so appends to one
// session serialise on that row and the log is the order they commit
// in. A writer whose parent is no longer the head is recorded as a
// branch and told so. A Session this store hands out is brought up to
// date with what other processes appended each time it is used through
// the store; between those calls it is a snapshot.
//
// Objects are keyed by hash, which spreads writes; sessions are keyed by
// ID, and a UUIDv7 ID is time-ordered, so a store creating sessions at a
// high rate concentrates those inserts on one split. The log is keyed
// by session and sequence and interleaved in its session, so a session's
// tail is one split, which is where its appends serialise anyway.
package spanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gs "cloud.google.com/go/spanner"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// DDL is the schema the store runs against, one statement per element,
// for a database admin client's CreateDatabase or UpdateDatabaseDdl.
// A hash column is named Digest, since HASH is reserved in GoogleSQL.
// There are no foreign keys, as the RFC's sketch has none: the rows are
// immutable and content-addressed, and the append's own rules keep the
// tables in step. Log and Prefix are interleaved in Sessions, so
// deleting a session's row deletes its log and its prefix with it.
var DDL = []string{
	`CREATE TABLE Contents (
		Digest  STRING(71) NOT NULL,
		Bytes   BYTES(MAX) NOT NULL,
		Written TIMESTAMP NOT NULL OPTIONS (allow_commit_timestamp = true),
	) PRIMARY KEY (Digest)`,
	`CREATE TABLE Entries (
		Digest   STRING(71) NOT NULL,
		Type     STRING(MAX) NOT NULL,
		Parent   STRING(71),
		Content  STRING(71) NOT NULL,
		Envelope BYTES(MAX) NOT NULL,
	) PRIMARY KEY (Digest)`,
	`CREATE INDEX EntriesByContent ON Entries (Content)`,
	`CREATE TABLE Sidecars (
		Content STRING(71) NOT NULL,
		Blob    STRING(71) NOT NULL,
	) PRIMARY KEY (Content, Blob)`,
	`CREATE INDEX SidecarsByBlob ON Sidecars (Blob)`,
	`CREATE TABLE Edges (
		Parent STRING(71) NOT NULL,
		Child  STRING(71) NOT NULL,
	) PRIMARY KEY (Parent, Child)`,
	`CREATE TABLE Sessions (
		Id            STRING(200) NOT NULL,
		Header        STRING(MAX) NOT NULL,
		Base          STRING(71),
		Head          STRING(71),
		Record        BOOL NOT NULL,
		Length        INT64 NOT NULL,
		CreatedAt     TIMESTAMP NOT NULL,
		Cwd           STRING(MAX) NOT NULL,
		ParentSession STRING(MAX) NOT NULL,
		Name          STRING(MAX) NOT NULL,
		SupersededBy  STRING(MAX) NOT NULL,
		Modified      TIMESTAMP NOT NULL OPTIONS (allow_commit_timestamp = true),
	) PRIMARY KEY (Id)`,
	`CREATE TABLE Log (
		Id     STRING(200) NOT NULL,
		Seq    INT64 NOT NULL,
		Digest STRING(71) NOT NULL,
	) PRIMARY KEY (Id, Seq), INTERLEAVE IN PARENT Sessions ON DELETE CASCADE`,
	`CREATE UNIQUE INDEX LogByDigest ON Log (Id, Digest), INTERLEAVE IN Sessions`,
	`CREATE INDEX LogByEntry ON Log (Digest)`,
	`CREATE TABLE Prefix (
		Id     STRING(200) NOT NULL,
		Digest STRING(71) NOT NULL,
	) PRIMARY KEY (Id, Digest), INTERLEAVE IN PARENT Sessions ON DELETE CASCADE`,
	`CREATE INDEX PrefixByEntry ON Prefix (Digest)`,
}

// ErrMirror is returned by Append and SetHead for a session this store
// holds as a mirror, which only exchange from the record may advance.
var ErrMirror = errors.New("spanner: session is a mirror here; only the record may advance it")

// ErrHeadMoved is returned by SetHead when the head is not the expected
// entry, and by Append when the caller moved the session's leaf through
// Session.Branch and another writer moved the head first: either way,
// the caller learns that another writer moved it.
var ErrHeadMoved = errors.New("spanner: head is not the expected entry")

// ErrBadName is returned for a session ID longer than the schema's
// key, or a hash that is not "sha256:" and 64 lowercase hex.
var ErrBadName = errors.New("spanner: not a usable name")

// ErrSynthetic is returned by Append for a leaf label carrying
// `synthetic`, which marks a projection's own marker and is never an
// entry a session appends.
var ErrSynthetic = errors.New("spanner: a synthetic leaf marker is the projection's, not the session's")

// ErrModified is returned when a session the store handed out was
// changed behind its back — an entry appended on it directly — so the
// store's record and the session disagree and nothing can be built on
// it.
var ErrModified = errors.New("spanner: session was modified outside the store")

// Store is a content-addressed store in one Spanner database.
type Store struct {
	client *gs.Client
	owned  bool
	mu     sync.Mutex
	open   map[string]*handle
}

// handle is a session this process has open. Its lock serialises this
// process's writers to the session; other processes serialise with it
// on the session row.
type handle struct {
	mu      sync.Mutex
	id      string
	session *agentsession.Session
	record  bool
	head    string // the head as the session row last had it
	count   int    // own entries the session holds, the log's length
	prefix  int    // entries on the path to the base
}

// Open connects to database, "projects/P/instances/I/databases/D", whose
// schema is [DDL]. The store closes the client it makes.
func Open(ctx context.Context, database string, opts ...option.ClientOption) (*Store, error) {
	c, err := gs.NewClient(ctx, database, opts...)
	if err != nil {
		return nil, fmt.Errorf("spanner: %w", err)
	}
	s := New(c)
	s.owned = true
	return s, nil
}

// New returns a store over a client the caller owns and closes.
func New(client *gs.Client) *Store {
	return &Store{client: client, open: map[string]*handle{}}
}

// Close forgets every open session and closes the client if Open made
// it. Nothing is held in the database, so there is nothing to release
// there.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = map[string]*handle{}
	if s.owned {
		s.client.Close()
	}
	return nil
}

// Release forgets a session this process has open, so the next Open
// reads it afresh.
func (s *Store) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, id)
}

// --- rows ---

// reader is what a read-only and a read-write transaction share.
type reader interface {
	ReadRow(ctx context.Context, table string, key gs.Key, columns []string) (*gs.Row, error)
	Read(ctx context.Context, table string, keys gs.KeySet, columns []string) *gs.RowIterator
	Query(ctx context.Context, statement gs.Statement) *gs.RowIterator
}

// sessionRow is a Sessions row.
type sessionRow struct {
	header agentsession.Header
	head   string
	record bool
	length int
}

var sessionColumns = []string{"Header", "Head", "Record", "Length"}

func readSession(ctx context.Context, r reader, id string) (sessionRow, error) {
	row, err := r.ReadRow(ctx, "Sessions", gs.Key{id}, sessionColumns)
	if gs.ErrCode(err) == codes.NotFound {
		return sessionRow{}, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if err != nil {
		return sessionRow{}, fmt.Errorf("spanner: session %s: %w", id, err)
	}
	var (
		hdr    string
		head   gs.NullString
		out    sessionRow
		length int64
	)
	if err := row.Columns(&hdr, &head, &out.record, &length); err != nil {
		return sessionRow{}, fmt.Errorf("spanner: session %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(hdr), &out.header); err != nil {
		return sessionRow{}, fmt.Errorf("spanner: session %s: header: %w", id, err)
	}
	out.head = head.StringVal
	out.length = int(length)
	return out, nil
}

func nullable(s string) gs.NullString {
	return gs.NullString{StringVal: s, Valid: s != ""}
}

// exists reports whether the table has a row under key, reading only
// the key's first column, which every table here names Digest or Id.
func exists(ctx context.Context, r reader, table string, key gs.Key) (bool, error) {
	col := "Digest"
	if table == "Sessions" {
		col = "Id"
	}
	_, err := r.ReadRow(ctx, table, key, []string{col})
	switch {
	case err == nil:
		return true, nil
	case gs.ErrCode(err) == codes.NotFound:
		return false, nil
	}
	return false, fmt.Errorf("spanner: %s: %w", table, err)
}

// readLog returns a session's own entries from sequence from on, in log
// order.
func readLog(ctx context.Context, r reader, id string, from int) ([]string, error) {
	keys := gs.KeyRange{Start: gs.Key{id, int64(from)}, End: gs.Key{id}, Kind: gs.ClosedClosed}
	var out []string
	err := r.Read(ctx, "Log", keys, []string{"Digest"}).Do(func(row *gs.Row) error {
		var h string
		if err := row.Columns(&h); err != nil {
			return err
		}
		out = append(out, h)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("spanner: log of %s: %w", id, err)
	}
	return out, nil
}

// readPrefix returns the entries on a session's path to its base, in no
// order.
func readPrefix(ctx context.Context, r reader, id string) ([]string, error) {
	var out []string
	err := r.Read(ctx, "Prefix", gs.Key{id}.AsPrefix(), []string{"Digest"}).Do(func(row *gs.Row) error {
		var h string
		if err := row.Columns(&h); err != nil {
			return err
		}
		out = append(out, h)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("spanner: prefix of %s: %w", id, err)
	}
	return out, nil
}

// object is an entry as the store holds it: the envelope and the body
// it names.
type object struct {
	parent   string
	content  string
	envelope []byte
	body     []byte
}

// loadObjects reads the envelopes of the given entries and the bodies
// they name. An entry the store does not hold is an error.
func loadObjects(ctx context.Context, r reader, hashes []string) (map[string]*object, error) {
	out := make(map[string]*object, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	keys := make([]gs.Key, 0, len(hashes))
	for _, h := range hashes {
		keys = append(keys, gs.Key{h})
	}
	err := r.Read(ctx, "Entries", gs.KeySetFromKeys(keys...), []string{"Digest", "Parent", "Content", "Envelope"}).Do(func(row *gs.Row) error {
		var (
			h      string
			parent gs.NullString
			o      object
		)
		if err := row.Columns(&h, &parent, &o.content, &o.envelope); err != nil {
			return err
		}
		o.parent = parent.StringVal
		out[h] = &o
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("spanner: entries: %w", err)
	}
	contents := map[string][]*object{}
	for _, h := range hashes {
		o, ok := out[h]
		if !ok {
			return nil, fmt.Errorf("spanner: entry %s: %w", h, agentsession.ErrNoEntry)
		}
		contents[o.content] = append(contents[o.content], o)
	}
	keys = keys[:0]
	for c := range contents {
		keys = append(keys, gs.Key{c})
	}
	err = r.Read(ctx, "Contents", gs.KeySetFromKeys(keys...), []string{"Digest", "Bytes"}).Do(func(row *gs.Row) error {
		var (
			h    string
			body []byte
		)
		if err := row.Columns(&h, &body); err != nil {
			return err
		}
		for _, o := range contents[h] {
			o.body = body
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("spanner: contents: %w", err)
	}
	for h, o := range out {
		if o.body == nil {
			return nil, fmt.Errorf("spanner: content %s of entry %s is missing", o.content, h)
		}
	}
	return out, nil
}

// pathTo orders the path from the root to id out of a set of objects
// that holds it.
func pathTo(objs map[string]*object, id string) ([]string, error) {
	var rev []string
	for id != "" {
		o, ok := objs[id]
		if !ok {
			return nil, fmt.Errorf("spanner: entry %s on the path is not held: %w", id, agentsession.ErrNoEntry)
		}
		rev = append(rev, id)
		id = o.parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

func lines(objs map[string]*object, hashes []string) ([][]byte, error) {
	out := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		o := objs[h]
		line, err := join(h, o.envelope, o.body)
		if err != nil {
			return nil, fmt.Errorf("spanner: entry %s: %w", h, err)
		}
		out = append(out, line)
	}
	return out, nil
}

// --- objects ---

// split takes an entry's encoded line apart into the envelope object
// the id hashes and the body the content hash hashes, both canonical.
func split(e agentsession.Entry) (env, body []byte, err error) {
	data, err := agentsession.MarshalEntry(e)
	if err != nil {
		return nil, nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, nil, err
	}
	b := e.Base()
	envObj := map[string]json.RawMessage{
		"type":    all["type"],
		"parent":  all["parent"],
		"ts":      all["ts"],
		"content": json.RawMessage(`"` + b.ContentHash() + `"`),
	}
	if envObj["parent"] == nil {
		envObj["parent"] = json.RawMessage("null")
	}
	if p, ok := all["parents"]; ok && len(b.Parents) > 0 {
		envObj["parents"] = p
	}
	for _, k := range []string{"id", "type", "parent", "parents", "ts", "content"} {
		delete(all, k)
	}
	envJSON, err := json.Marshal(envObj)
	if err != nil {
		return nil, nil, err
	}
	bodyJSON, err := json.Marshal(all)
	if err != nil {
		return nil, nil, err
	}
	if env, err = jcs.Transform(envJSON); err != nil {
		return nil, nil, err
	}
	if body, err = jcs.Transform(bodyJSON); err != nil {
		return nil, nil, err
	}
	return env, body, nil
}

// join rebuilds an entry's line from its envelope object and body, with
// the id set, for a reader to verify and decode.
func join(id string, env, body []byte) ([]byte, error) {
	var e, b map[string]json.RawMessage
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	delete(e, "content")
	for k, v := range e {
		b[k] = v
	}
	b["id"] = json.RawMessage(`"` + id + `"`)
	return json.Marshal(b)
}

// sidecarRef matches a sidecar reference inside a content, the URL RFC
// 0001 gives an item that names a blob beside the file.
var sidecarRef = regexp.MustCompile(`sidecar:sha256:[0-9a-f]{64}`)

// blobsNamedBy returns the blob hashes the content names through sidecar
// references, each once.
func blobsNamedBy(content []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range sidecarRef.FindAll(content, -1) {
		b := strings.TrimPrefix(string(m), "sidecar:")
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// putEntry buffers the writes that store an entry's objects inside txn,
// each only when the store lacks it, since an object is immutable and
// storing one already present is a no-op. It returns the sidecar blobs
// the body names that the store does not hold, which the format lets a
// store report rather than refuse. A transaction does not read its own
// buffered writes, so buffered records the objects this transaction has
// already inserted, and an import storing one body twice inserts it
// once.
func putEntry(ctx context.Context, txn *gs.ReadWriteTransaction, e agentsession.Entry, buffered map[string]bool) ([]string, error) {
	env, body, err := split(e)
	if err != nil {
		return nil, err
	}
	b := e.Base()
	var ms []*gs.Mutation
	var missing []string
	held := buffered[b.ContentHash()]
	if !held {
		if held, err = exists(ctx, txn, "Contents", gs.Key{b.ContentHash()}); err != nil {
			return nil, err
		}
	}
	blobs := blobsNamedBy(body)
	if !held {
		buffered[b.ContentHash()] = true
		ms = append(ms, gs.Insert("Contents", []string{"Digest", "Bytes", "Written"}, []any{b.ContentHash(), body, gs.CommitTimestamp}))
		for _, blob := range blobs {
			ms = append(ms, gs.InsertOrUpdate("Sidecars", []string{"Content", "Blob"}, []any{b.ContentHash(), blob}))
		}
	}
	// Reading each blob inside the transaction is what keeps a sweep
	// from removing it under the append: the sweep's read of the blob's
	// sidecar rows and this read of the blob conflict, so one of the two
	// transactions sees the other.
	for _, blob := range blobs {
		ok, err := exists(ctx, txn, "Contents", gs.Key{blob})
		if err != nil {
			return nil, err
		}
		if !ok && !buffered[blob] {
			missing = append(missing, blob)
		}
	}
	held = buffered[b.ID]
	if !held {
		if held, err = exists(ctx, txn, "Entries", gs.Key{b.ID}); err != nil {
			return nil, err
		}
	}
	if !held {
		buffered[b.ID] = true
		ms = append(ms, gs.Insert("Entries", []string{"Digest", "Type", "Parent", "Content", "Envelope"},
			[]any{b.ID, e.EntryType(), nullable(b.Parent), b.ContentHash(), env}))
		if b.Parent != "" {
			ms = append(ms, gs.InsertOrUpdate("Edges", []string{"Parent", "Child"}, []any{b.Parent, b.ID}))
		}
	}
	return missing, txn.BufferWrite(ms)
}

// --- media blobs ---

// PutBlob stores a media blob under its hash and returns the hash. An
// item names it with a sidecar: URL carrying the hash, and a projection
// of a sidecar session writes it beside the file as the file named by
// the hex digest. A blob is a content, held once. Putting a blob the
// store holds stamps it written now, so a sweep's grace starts again
// for a blob a writer is about to name.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	hash := agentsession.HashBytes(data)
	_, err := s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
		held, err := exists(ctx, txn, "Contents", gs.Key{hash})
		if err != nil {
			return err
		}
		if held {
			return txn.BufferWrite([]*gs.Mutation{gs.Update("Contents", []string{"Digest", "Written"}, []any{hash, gs.CommitTimestamp})})
		}
		return txn.BufferWrite([]*gs.Mutation{gs.Insert("Contents", []string{"Digest", "Bytes", "Written"}, []any{hash, data, gs.CommitTimestamp})})
	})
	if err != nil {
		return "", fmt.Errorf("spanner: store blob: %w", err)
	}
	return hash, nil
}

// Blob returns a media blob by its hash. Bytes that do not hash to
// their name are corrupt and are not served.
func (s *Store) Blob(ctx context.Context, hash string) ([]byte, error) {
	if !agentsession.ValidHash(hash) {
		return nil, fmt.Errorf("%w: hash %q", ErrBadName, hash)
	}
	row, err := s.client.Single().ReadRow(ctx, "Contents", gs.Key{hash}, []string{"Bytes"})
	if err != nil {
		return nil, fmt.Errorf("spanner: blob %s: %w", hash, err)
	}
	var data []byte
	if err := row.Columns(&data); err != nil {
		return nil, fmt.Errorf("spanner: blob %s: %w", hash, err)
	}
	if agentsession.HashBytes(data) != hash {
		return nil, fmt.Errorf("spanner: blob %s: bytes do not hash to their name", hash)
	}
	return data, nil
}

// --- sessions ---

func validSessionID(id string) error {
	if len(id) > 200 {
		return fmt.Errorf("%w: session id of %d bytes", ErrBadName, len(id))
	}
	return nil
}

// mediaOf returns a header's media with the format's default applied,
// so "" and "inline" compare as one value.
func mediaOf(h agentsession.Header) string {
	if h.Media == "" {
		return agentsession.MediaInline
	}
	return h.Media
}

// sessionMutation is the insert of a new Sessions row.
func sessionMutation(h agentsession.Header, head string, record bool, length int, name, supersededBy string) (*gs.Mutation, error) {
	hdr, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if hdr, err = jcs.Transform(hdr); err != nil {
		return nil, err
	}
	return gs.Insert("Sessions",
		[]string{"Id", "Header", "Base", "Head", "Record", "Length", "CreatedAt", "Cwd", "ParentSession", "Name", "SupersededBy", "Modified"},
		[]any{h.ID, string(hdr), nullable(h.Base), nullable(head), record, int64(length), h.CreatedAt, h.CWD, h.ParentSession, name, supersededBy, gs.CommitTimestamp}), nil
}

// holderOf returns a session that holds the entry: named when it holds
// it, else one whose log holds it, else one whose prefix holds it, the
// first by ID when several do, and whether the entry is in that
// session's log.
func holderOf(ctx context.Context, r reader, named, entry string) (string, bool, error) {
	query := func(sql string) ([]string, error) {
		var ids []string
		err := r.Query(ctx, gs.Statement{SQL: sql, Params: map[string]any{"h": entry}}).Do(func(row *gs.Row) error {
			var id string
			if err := row.Columns(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		})
		return ids, err
	}
	owners, err := query(`SELECT Id FROM Log WHERE Digest = @h ORDER BY Id`)
	if err != nil {
		return "", false, fmt.Errorf("spanner: %w", err)
	}
	onPrefix, err := query(`SELECT Id FROM Prefix WHERE Digest = @h ORDER BY Id`)
	if err != nil {
		return "", false, fmt.Errorf("spanner: %w", err)
	}
	for _, id := range owners {
		if id == named {
			return id, true, nil
		}
	}
	for _, id := range onPrefix {
		if id == named {
			return id, false, nil
		}
	}
	if len(owners) > 0 {
		return owners[0], true, nil
	}
	if len(onPrefix) > 0 {
		return onPrefix[0], false, nil
	}
	return "", false, nil
}

// assemble builds a session from a header, the prefix lines and the own
// lines through the format's reader, so every line is verified as a
// file's would be, and sets the head.
func assemble(h agentsession.Header, prefix, own [][]byte, head string) (*agentsession.Session, error) {
	var buf bytes.Buffer
	hdr, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	buf.Write(hdr)
	buf.WriteByte('\n')
	for _, l := range prefix {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	for _, l := range own {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	sess, err := agentsession.Read(&buf)
	if err != nil {
		return nil, fmt.Errorf("spanner: session %s: %w", h.ID, err)
	}
	if err := setLeaf(sess, head); err != nil {
		return nil, fmt.Errorf("spanner: session %s: head: %w", h.ID, err)
	}
	return sess, nil
}

// setLeaf puts the session's leaf on id, or clears it for "".
func setLeaf(sess *agentsession.Session, id string) error {
	if id == "" {
		sess.ResetLeaf()
		return nil
	}
	return sess.Branch(id)
}

// Create implements agentsession.Store. A header whose Base is set makes
// a fork: the store must hold the base, in a session's log or on its
// prefix; the base may not be a leaf label; the media, when set, must
// be that of the session holding the base, and is taken from it when
// not; ParentSession defaults to that session; and the head starts at
// the base. Nothing is copied: the fork's prefix rows name the entries
// its path shares with the origin. The session is the record here.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	var (
		sess   *agentsession.Session
		prefix []string
	)
	if h.Base != "" {
		var err error
		if h, prefix, sess, err = s.forkFrom(ctx, h); err != nil {
			return nil, err
		}
	} else {
		sess = agentsession.New(h)
		h = sess.Header()
	}
	if err := validSessionID(h.ID); err != nil {
		return nil, err
	}
	_, err := s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
		if held, err := exists(ctx, txn, "Sessions", gs.Key{h.ID}); err != nil {
			return err
		} else if held {
			return fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
		}
		if h.Base != "" {
			// Read again inside the transaction, so a sweep cannot take the
			// path between the read that built it and this commit: the
			// sweep's check of the base conflicts with this one.
			if holder, _, err := holderOf(ctx, txn, h.ParentSession, h.Base); err != nil {
				return err
			} else if holder == "" {
				return fmt.Errorf("%w: base %s is no longer held by this store", agentsession.ErrNoEntry, h.Base)
			}
		}
		m, err := sessionMutation(h, h.Base, true, 0, sess.Name(), sess.SupersededBy())
		if err != nil {
			return err
		}
		ms := []*gs.Mutation{m}
		for _, p := range prefix {
			ms = append(ms, gs.Insert("Prefix", []string{"Id", "Digest"}, []any{h.ID, p}))
		}
		return txn.BufferWrite(ms)
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open[h.ID] = &handle{id: h.ID, session: sess, record: true, head: h.Base, prefix: len(prefix)}
	return sess, nil
}

// forkFrom resolves a fork's header against the session holding its
// base and builds the fork in memory, verified line by line.
func (s *Store) forkFrom(ctx context.Context, h agentsession.Header) (agentsession.Header, []string, *agentsession.Session, error) {
	txn := s.client.ReadOnlyTransaction()
	defer txn.Close()
	holder, _, err := holderOf(ctx, txn, h.ParentSession, h.Base)
	if err != nil {
		return h, nil, nil, err
	}
	if holder == "" {
		return h, nil, nil, fmt.Errorf("%w: base %s is not held by this store", agentsession.ErrNoEntry, h.Base)
	}
	origin, err := readSession(ctx, txn, holder)
	if err != nil {
		return h, nil, nil, err
	}
	if h.ParentSession == "" {
		h.ParentSession = holder
	}
	if h.Media != "" && mediaOf(h) != mediaOf(origin.header) {
		return h, nil, nil, errors.New("spanner: a fork's media must equal its origin's")
	}
	h.Media = origin.header.Media
	if h.Payload == "" {
		h.Payload = origin.header.Payload
	}
	// The path to the base lies within the holder's log and prefix.
	own, err := readLog(ctx, txn, holder, 1)
	if err != nil {
		return h, nil, nil, err
	}
	pre, err := readPrefix(ctx, txn, holder)
	if err != nil {
		return h, nil, nil, err
	}
	objs, err := loadObjects(ctx, txn, append(own, pre...))
	if err != nil {
		return h, nil, nil, err
	}
	path, err := pathTo(objs, h.Base)
	if err != nil {
		return h, nil, nil, err
	}
	pathLines, err := lines(objs, path)
	if err != nil {
		return h, nil, nil, err
	}
	base, err := agentsession.UnmarshalEntry(pathLines[len(pathLines)-1])
	if err != nil {
		return h, nil, nil, err
	}
	if isLeafLabel(base) {
		return h, nil, nil, errors.New("spanner: a base may not be a leaf label")
	}
	h = agentsession.New(h).Header()
	sess, err := assemble(h, pathLines, nil, h.Base)
	if err != nil {
		return h, nil, nil, err
	}
	return h, path, sess, nil
}

func isLeafLabel(e agentsession.Entry) bool {
	l, ok := e.(*agentsession.LabelEntry)
	return ok && l.Label != nil && *l.Label == agentsession.LeafLabel
}

// isSynthetic reports whether a label carries synthetic: true, the
// projection's own marker.
func isSynthetic(l *agentsession.LabelEntry) bool {
	raw, ok := l.Unknown["synthetic"]
	if !ok {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

// Open implements agentsession.Store. A session this process already
// has open is brought up to date with what other writers appended, and
// its leaf follows the head unless the caller moved it.
func (s *Store) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	h, err := s.handle(ctx, id)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := s.follow(ctx, h); err != nil {
		return nil, err
	}
	return h.session, nil
}

// handle returns the session's handle, loading it when this process
// does not have it open.
func (s *Store) handle(ctx context.Context, id string) (*handle, error) {
	if err := validSessionID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	h, ok := s.open[id]
	s.mu.Unlock()
	if ok {
		return h, nil
	}
	h, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.open[id]; ok {
		return prev, nil // another goroutine loaded it first
	}
	s.open[id] = h
	return h, nil
}

// load reads a session from one snapshot: its row, its prefix and its
// log, with every object, verified through the format's reader.
func (s *Store) load(ctx context.Context, id string) (*handle, error) {
	txn := s.client.ReadOnlyTransaction()
	defer txn.Close()
	row, err := readSession(ctx, txn, id)
	if err != nil {
		return nil, err
	}
	own, err := readLog(ctx, txn, id, 1)
	if err != nil {
		return nil, err
	}
	var pre []string
	if row.header.Base != "" {
		if pre, err = readPrefix(ctx, txn, id); err != nil {
			return nil, err
		}
	}
	objs, err := loadObjects(ctx, txn, append(append([]string(nil), own...), pre...))
	if err != nil {
		return nil, err
	}
	var prefixLines [][]byte
	var path []string
	if row.header.Base != "" {
		if path, err = pathTo(objs, row.header.Base); err != nil {
			return nil, err
		}
		if prefixLines, err = lines(objs, path); err != nil {
			return nil, err
		}
	}
	ownLines, err := lines(objs, own)
	if err != nil {
		return nil, err
	}
	sess, err := assemble(row.header, prefixLines, ownLines, row.head)
	if err != nil {
		return nil, err
	}
	return &handle{id: id, session: sess, record: row.record, head: row.head, count: len(own), prefix: len(path)}, nil
}

// untouched checks that the session holds exactly the entries the store
// put in it: an entry appended on the session directly is in no log,
// and nothing may build on it.
func (h *handle) untouched() error {
	if n := h.session.Len(); n != h.prefix+h.count {
		return fmt.Errorf("%w: %d entries, %d from the store", ErrModified, n, h.prefix+h.count)
	}
	return nil
}

// refresh brings the handle up to date with the session row: it folds
// in the entries other writers appended since, in log order, and takes
// the head and the mark. The session's leaf is left wherever the fold
// put it; the caller puts it where it belongs. A session that can no
// longer be brought in step — deleted, or replaced under the same ID —
// is dropped, so the next Open reads it afresh.
func (s *Store) refresh(ctx context.Context, h *handle) error {
	txn := s.client.ReadOnlyTransaction()
	defer txn.Close()
	row, err := readSession(ctx, txn, h.id)
	if err != nil {
		s.drop(h)
		return err
	}
	if row.length < h.count || row.header.ID != h.session.ID() || !row.header.CreatedAt.Equal(h.session.Header().CreatedAt) {
		s.drop(h)
		return fmt.Errorf("spanner: session %s was replaced; open it again", h.id)
	}
	if row.length > h.count {
		hashes, err := readLog(ctx, txn, h.id, h.count+1)
		if err != nil {
			return err
		}
		objs, err := loadObjects(ctx, txn, hashes)
		if err != nil {
			return err
		}
		ls, err := lines(objs, hashes)
		if err != nil {
			return err
		}
		for i, line := range ls {
			e, err := agentsession.UnmarshalEntry(line)
			if err == nil {
				// A root names no parent, and an entry with none takes
				// the leaf's; clear it so the root stays one.
				if e.Base().Parent == "" {
					h.session.ResetLeaf()
				}
				_, err = h.session.Commit(e)
			}
			if err != nil {
				s.drop(h)
				return fmt.Errorf("spanner: session %s: entry %s another writer appended: %w", h.id, hashes[i], err)
			}
			h.count++
		}
	}
	h.head = row.head
	h.record = row.record
	return nil
}

// follow refreshes the handle and puts the leaf on the head, unless the
// caller moved the leaf since the store last set it, in which case it
// stays where the caller put it.
func (s *Store) follow(ctx context.Context, h *handle) error {
	if err := h.untouched(); err != nil {
		return err
	}
	leaf, seen := h.session.Leaf(), h.head
	if err := s.refresh(ctx, h); err != nil {
		return err
	}
	if leaf != seen {
		return setLeaf(h.session, leaf)
	}
	return setLeaf(h.session, h.head)
}

func (s *Store) drop(h *handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[h.id] == h {
		delete(s.open, h.id)
	}
}

// errStale is returned from inside a transaction that found the session
// row moved since the handle was refreshed; the append starts over.
var errStale = errors.New("spanner: session moved since it was read")

// Append implements agentsession.Store; see Write for what it reports.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	r, err := s.Write(ctx, sessionID, e)
	return r.ID, err
}

// Write appends an entry and reports what happened, as RFC 0002 asks a
// store to: whether the entry continued the head, branched, was already
// held, or as a leaf label moved the head or could not. Every append is
// durable when Write returns.
//
// The entry's parent defaults to the session's leaf as the caller last
// saw it, not to whatever another writer has made the head since, so a
// writer that composed its entry against a head another writer moved
// is recorded as a branch and told so, and the head stays with the
// other writer. After the append the session's leaf is the head.
//
// A leaf the caller moved through Session.Branch since the store last
// set it is a head move, made by compare-and-swap from the head the
// caller last saw, in the same transaction as the append; if another
// writer moved the head first, Write returns ErrHeadMoved, appends
// nothing, and puts the leaf on the head the other writer left.
//
// A session held as a mirror is refused, as is a leaf label carrying
// `synthetic`.
func (s *Store) Write(ctx context.Context, sessionID string, e agentsession.Entry) (agentsession.Result, error) {
	if l, ok := e.(*agentsession.LabelEntry); ok && isSynthetic(l) {
		return agentsession.Result{}, ErrSynthetic
	}
	h, err := s.handle(ctx, sessionID)
	if err != nil {
		return agentsession.Result{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.untouched(); err != nil {
		return agentsession.Result{}, err
	}
	leaf, seen := h.session.Leaf(), h.head
	moved := leaf != seen
	for {
		if err := ctx.Err(); err != nil {
			return agentsession.Result{}, err
		}
		if err := s.refresh(ctx, h); err != nil {
			return agentsession.Result{}, err
		}
		if !h.record {
			setLeaf(h.session, h.head)
			return agentsession.Result{}, fmt.Errorf("%w: %s", ErrMirror, sessionID)
		}
		if moved && h.head != seen {
			setLeaf(h.session, h.head)
			return agentsession.Result{}, fmt.Errorf("%w: head is %s", ErrHeadMoved, orNone(h.head))
		}
		if moved {
			if err := mayRestOn(h.session, leaf); err != nil {
				setLeaf(h.session, h.head)
				return agentsession.Result{}, err
			}
		}
		if err := setLeaf(h.session, leaf); err != nil {
			return agentsession.Result{}, err
		}
		r, err := h.session.Prepare(e)
		if err != nil {
			setLeaf(h.session, h.head)
			return agentsession.Result{}, err
		}
		if r.Outcome == agentsession.Held {
			setLeaf(h.session, h.head)
			return r, nil
		}
		// The head this append is judged against: the one the caller
		// moved to, or the one in the row.
		against := h.head
		if moved {
			against = leaf
		}
		head := against
		b := e.Base()
		switch {
		case isLeafLabel(e):
			if r.Outcome == agentsession.LeafMoved {
				head = e.(*agentsession.LabelEntry).Target
			}
		case b.Parent == against:
			r.Outcome = agentsession.Continued
			head = b.ID
		default:
			r.Outcome = agentsession.Branched
		}
		name, next := h.session.Name(), h.session.SupersededBy()
		switch v := e.(type) {
		case *agentsession.InfoEntry:
			if v.Name != "" {
				name = v.Name
			}
		case *agentsession.LinkEntry:
			if v.Rel == agentsession.RelContinuedIn && v.Session != "" {
				next = v.Session
			}
		}
		count, rowHead := h.count, h.head
		var unresolved []string
		_, err = s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
			row, err := readSession(ctx, txn, sessionID)
			if err != nil {
				return err
			}
			if row.length != count || row.head != rowHead || !row.record {
				return errStale
			}
			if unresolved, err = putEntry(ctx, txn, e, map[string]bool{}); err != nil {
				return err
			}
			return txn.BufferWrite([]*gs.Mutation{
				gs.Insert("Log", []string{"Id", "Seq", "Digest"}, []any{sessionID, int64(count + 1), b.ID}),
				gs.Update("Sessions", []string{"Id", "Head", "Length", "Name", "SupersededBy", "Modified"},
					[]any{sessionID, nullable(head), int64(count + 1), name, next, gs.CommitTimestamp}),
			})
		})
		if errors.Is(err, errStale) {
			continue
		}
		if err != nil {
			setLeaf(h.session, h.head)
			return agentsession.Result{}, err
		}
		// The append is durable from here, and nothing after this point
		// reports it as failed.
		r.Unresolved = unresolved
		h.head = head
		if _, err := h.session.Commit(e); err != nil {
			s.drop(h)
			r.Reopen = true
			return r, nil
		}
		h.count++
		if err := setLeaf(h.session, head); err != nil {
			s.drop(h)
			r.Reopen = true
		}
		return r, nil
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// mayRestOn holds a head target to the rule: the base or an own entry,
// and not a leaf label.
func mayRestOn(sess *agentsession.Session, id string) error {
	if id == "" {
		if sess.Header().Base != "" {
			return fmt.Errorf("%w: a session with a base has a head", agentsession.ErrNoEntry)
		}
		return nil
	}
	e, ok := sess.Entry(id)
	if !ok {
		return fmt.Errorf("%w: %s", agentsession.ErrNoEntry, id)
	}
	if b := sess.Header().Base; b != "" && id != b && sess.Prefix(id) {
		return fmt.Errorf("%w: %s is on the prefix above the base", agentsession.ErrNoEntry, id)
	}
	if isLeafLabel(e) {
		return errors.New("spanner: the head never rests on a leaf label")
	}
	return nil
}

// SetHead moves a session's head from expected to the entry to, or
// returns ErrHeadMoved when the head is not expected. "" is a valid
// expected value for a session with no head, and a valid target for a
// session with no base. The target must be the base or an own entry and
// not a leaf label; a mirror is refused. The session's leaf is the head
// afterwards, whichever way it went.
func (s *Store) SetHead(ctx context.Context, sessionID, expected, to string) error {
	h, err := s.handle(ctx, sessionID)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.untouched(); err != nil {
		return err
	}
	for {
		if err := s.refresh(ctx, h); err != nil {
			return err
		}
		if err := s.setHeadOnce(ctx, h, expected, to); !errors.Is(err, errStale) {
			if serr := setLeaf(h.session, h.head); err == nil {
				err = serr
			}
			return err
		}
	}
}

func (s *Store) setHeadOnce(ctx context.Context, h *handle, expected, to string) error {
	if !h.record {
		return fmt.Errorf("%w: %s", ErrMirror, h.id)
	}
	if h.head != expected {
		return fmt.Errorf("%w: head is %s", ErrHeadMoved, orNone(h.head))
	}
	if err := mayRestOn(h.session, to); err != nil {
		return err
	}
	count, head := h.count, h.head
	_, err := s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
		row, err := readSession(ctx, txn, h.id)
		if err != nil {
			return err
		}
		if row.length != count || row.head != head || !row.record {
			return errStale
		}
		return txn.BufferWrite([]*gs.Mutation{gs.Update("Sessions", []string{"Id", "Head", "Modified"},
			[]any{h.id, nullable(to), gs.CommitTimestamp})})
	})
	if err != nil {
		return err
	}
	h.head = to
	return nil
}

// Head returns the session's head as the store has it now, or "" for a
// session with none.
func (s *Store) Head(ctx context.Context, sessionID string) (string, error) {
	row, err := readSession(ctx, s.client.Single(), sessionID)
	if err != nil {
		return "", err
	}
	return row.head, nil
}

// Record reports whether this store is the record for the session, as
// against a mirror of it.
func (s *Store) Record(ctx context.Context, sessionID string) (bool, error) {
	row, err := readSession(ctx, s.client.Single(), sessionID)
	if err != nil {
		return false, err
	}
	return row.record, nil
}

// DeclareRecord makes this store the record for a session it holds as
// a mirror: what a mirror does when the record deleted the session
// without handing it over, or an importer does for a file it knows to
// be the only copy.
func (s *Store) DeclareRecord(ctx context.Context, sessionID string) error {
	_, err := s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
		if _, err := readSession(ctx, txn, sessionID); err != nil {
			return err
		}
		return txn.BufferWrite([]*gs.Mutation{gs.Update("Sessions", []string{"Id", "Record", "Modified"},
			[]any{sessionID, true, gs.CommitTimestamp})})
	})
	return err
}

// List implements agentsession.Store. Names and successors are columns
// the append keeps, so every summary carries them.
func (s *Store) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return func(yield func(agentsession.Summary, error) bool) {
		var where []string
		params := map[string]any{}
		if f.CWD != "" {
			where = append(where, "Cwd = @cwd")
			params["cwd"] = f.CWD
		}
		if f.ParentSession != "" {
			where = append(where, "ParentSession = @parent")
			params["parent"] = f.ParentSession
		}
		if !f.After.IsZero() {
			where = append(where, "CreatedAt > @after")
			params["after"] = f.After
		}
		if !f.Before.IsZero() {
			where = append(where, "CreatedAt < @before")
			params["before"] = f.Before
		}
		if f.Current {
			where = append(where, "SupersededBy = ''")
		}
		sql := "SELECT Header, Name, SupersededBy, Modified FROM Sessions"
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		sql += " ORDER BY CreatedAt DESC, Id"
		if f.Limit > 0 {
			sql += " LIMIT @limit"
			params["limit"] = int64(f.Limit)
		}
		it := s.client.Single().Query(ctx, gs.Statement{SQL: sql, Params: params})
		defer it.Stop()
		for {
			row, err := it.Next()
			if err == iterator.Done {
				return
			}
			if err != nil {
				yield(agentsession.Summary{}, fmt.Errorf("spanner: list: %w", err))
				return
			}
			var (
				hdr string
				sum agentsession.Summary
			)
			if err := row.Columns(&hdr, &sum.Name, &sum.SupersededBy, &sum.Modified); err != nil {
				if !yield(agentsession.Summary{}, fmt.Errorf("spanner: list: %w", err)) {
					return
				}
				continue
			}
			if err := json.Unmarshal([]byte(hdr), &sum.Header); err != nil {
				if !yield(agentsession.Summary{}, fmt.Errorf("spanner: list: header: %w", err)) {
					return
				}
				continue
			}
			if !yield(sum, nil) {
				return
			}
		}
	}
}

// Delete implements agentsession.Store: it removes the session's row,
// and with it its log and prefix. Objects stay; what no log and no
// prefix needs is swept by Sweep.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := validSessionID(id); err != nil {
		return err
	}
	_, err := s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
		if held, err := exists(ctx, txn, "Sessions", gs.Key{id}); err != nil {
			return err
		} else if !held {
			return fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
		}
		return txn.BufferWrite([]*gs.Mutation{gs.Delete("Sessions", gs.Key{id})})
	})
	if err != nil {
		return err
	}
	s.Release(id)
	return nil
}

// --- projection ---

// Project writes the session as an RFC 0001 file: the header, the
// prefix, the own entries in log order, and a synthetic leaf marker
// when the file's resume rule would not land a reader on the head.
func (s *Store) Project(ctx context.Context, w io.Writer, sessionID string) error {
	sess, head, err := s.snapshot(ctx, sessionID)
	if err != nil {
		return err
	}
	return project(w, sess, head)
}

// snapshot brings the session up to date and returns it with its head.
func (s *Store) snapshot(ctx context.Context, sessionID string) (*agentsession.Session, string, error) {
	h, err := s.handle(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := s.follow(ctx, h); err != nil {
		return nil, "", err
	}
	return h.session, h.head, nil
}

// ProjectDir writes the session's file into dir as <id>.jsonl and, for a
// sidecar session, every blob its entries name as <id>/<hex>, so the
// projection is self-contained as RFC 0001 requires. It returns the
// file's path.
func (s *Store) ProjectDir(ctx context.Context, dir, sessionID string) (string, error) {
	sess, head, err := s.snapshot(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := project(&buf, sess, head); err != nil {
		return "", err
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	if mediaOf(sess.Header()) != agentsession.MediaSidecar {
		return path, nil
	}
	blobDir := filepath.Join(dir, sessionID)
	for _, e := range sess.Entries() {
		_, body, err := split(e)
		if err != nil {
			return "", err
		}
		for _, b := range blobsNamedBy(body) {
			data, err := s.Blob(ctx, b)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(blobDir, 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(blobDir, strings.TrimPrefix(b, agentsession.HashPrefix)), data, 0o644); err != nil {
				return "", err
			}
		}
	}
	return path, nil
}

// project is Project over a session in memory with the given head.
func project(w io.Writer, sess *agentsession.Session, head string) error {
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, sess); err != nil {
		return err
	}
	check, err := agentsession.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	if head != "" && check.Leaf() != head {
		last := sess.Entries()[sess.Len()-1]
		headEntry, _ := sess.Entry(head)
		marker := agentsession.NewLabelEntry(head, agentsession.LeafLabel)
		marker.Parent = last.Base().ID
		marker.Timestamp = headEntry.Base().Timestamp
		marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage("true")}
		if _, err := check.Append(marker); err != nil {
			return err
		}
		buf.Reset()
		if err := agentsession.Write(&buf, check); err != nil {
			return err
		}
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// Import reads an RFC 0001 file into the store as a mirror of the
// session it holds, unless asRecord says this file is the only copy. It
// is held to a push's checks: every line verifies, a truncated last
// line is refused, a redacted header is refused, every own entry hangs
// from the base or another own entry, and the media matches that of a
// held session whose own entries include the base. A trailing synthetic
// marker names the head and is discarded; prefix entries are stored and
// join the prefix, not the log; own entries join the log in file order.
// A session the store already holds is refused. The whole import is one
// transaction, so a file is imported whole or not at all, and a file
// larger than one Spanner commit allows is refused by Spanner.
func (s *Store) Import(ctx context.Context, r io.Reader, asRecord bool) (*agentsession.Session, error) {
	sess, err := agentsession.Read(r)
	if err != nil {
		return nil, err
	}
	if t := sess.Truncated(); t != nil {
		return nil, fmt.Errorf("spanner: import: line %d is cut short: %w", t.Line, t.Err)
	}
	h := sess.Header()
	if h.Redacted {
		return nil, errors.New("spanner: a redacted projection is a record to read, not one to hold")
	}
	if migrated, unresolved := sess.Migrated(); migrated && len(unresolved) > 0 {
		return nil, agentsession.ErrUnresolvedMigration
	}
	if err := validSessionID(h.ID); err != nil {
		return nil, err
	}
	entries := sess.Entries()
	var stored, own, prefix []agentsession.Entry
	for i, e := range entries {
		if l, ok := e.(*agentsession.LabelEntry); ok && isSynthetic(l) {
			if i == len(entries)-1 && isLeafLabel(l) {
				continue // the projection's marker, discarded
			}
			return nil, fmt.Errorf("%w: at line %d", ErrSynthetic, i+2)
		}
		stored = append(stored, e)
		if sess.Prefix(e.Base().ID) {
			prefix = append(prefix, e)
			continue
		}
		// An own entry hangs from the base or another own entry, or from
		// null in a baseless session.
		p := e.Base().Parent
		if h.Base != "" && p != h.Base && (p == "" || sess.Prefix(p)) {
			return nil, fmt.Errorf("spanner: import: own entry %s hangs from %s, above the base", e.Base().ID, orNone(p))
		}
		own = append(own, e)
	}
	head := sess.Leaf()
	record := asRecord
	_, err = s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
		if held, err := exists(ctx, txn, "Sessions", gs.Key{h.ID}); err != nil {
			return err
		} else if held {
			return fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
		}
		if h.Base != "" {
			if holder, inLog, err := holderOf(ctx, txn, h.ParentSession, h.Base); err != nil {
				return err
			} else if holder != "" && inLog {
				origin, err := readSession(ctx, txn, holder)
				if err != nil {
					return err
				}
				if mediaOf(origin.header) != mediaOf(h) {
					return errors.New("spanner: import: media differs from the session holding the base")
				}
			}
		}
		buffered := map[string]bool{}
		for _, e := range stored {
			if _, err := putEntry(ctx, txn, e, buffered); err != nil {
				return err
			}
		}
		m, err := sessionMutation(h, head, record, len(own), sess.Name(), sess.SupersededBy())
		if err != nil {
			return err
		}
		ms := []*gs.Mutation{m}
		for _, e := range prefix {
			ms = append(ms, gs.Insert("Prefix", []string{"Id", "Digest"}, []any{h.ID, e.Base().ID}))
		}
		for i, e := range own {
			ms = append(ms, gs.Insert("Log", []string{"Id", "Seq", "Digest"}, []any{h.ID, int64(i + 1), e.Base().ID}))
		}
		return txn.BufferWrite(ms)
	})
	if err != nil {
		return nil, err
	}
	// What the store returns is what it holds: the session rebuilt from
	// the objects and the log, marker gone, head from the row.
	s.Release(h.ID)
	return s.Open(ctx, h.ID)
}

// --- sweep ---

// sweepBatch is how many candidates one sweep transaction re-checks and
// removes.
const sweepBatch = 500

// Sweep removes objects nothing needs, following references down: an
// envelope is kept while a log or a prefix names it, a content while a
// kept envelope names it, and a media blob while a kept content names
// it through a sidecar: URL. It never sweeps by reachability from heads,
// so abandoned branches stay.
//
// Every removal re-checks its object inside the transaction that
// removes it, and an append reads what it builds on inside its own, so
// the two conflict and Spanner serialises them: a sweep runs alongside
// live writers without a lock. An entry and its content are committed
// together, so neither is ever unreferenced while a writer needs it; a
// blob is the exception, stored by PutBlob before any content names it,
// so contents written less than grace ago are spared, and grace must
// exceed the longest time a writer takes between PutBlob and the append
// that names the blob. It returns how many objects went.
func (s *Store) Sweep(ctx context.Context, grace time.Duration) (int, error) {
	swept := 0
	n, err := s.sweepPass(ctx,
		`SELECT e.Digest FROM Entries e
		 WHERE NOT EXISTS (SELECT 1 FROM Log l WHERE l.Digest = e.Digest)
		   AND NOT EXISTS (SELECT 1 FROM Prefix p WHERE p.Digest = e.Digest)`,
		nil,
		`SELECT e.Digest, e.Parent FROM Entries e
		 WHERE e.Digest IN UNNEST(@hs)
		   AND NOT EXISTS (SELECT 1 FROM Log l WHERE l.Digest = e.Digest)
		   AND NOT EXISTS (SELECT 1 FROM Prefix p WHERE p.Digest = e.Digest)`,
		func(row *gs.Row) ([]*gs.Mutation, error) {
			var (
				h      string
				parent gs.NullString
			)
			if err := row.Columns(&h, &parent); err != nil {
				return nil, err
			}
			ms := []*gs.Mutation{gs.Delete("Entries", gs.Key{h})}
			if parent.Valid {
				ms = append(ms, gs.Delete("Edges", gs.Key{parent.StringVal, h}))
			}
			return ms, nil
		})
	swept += n
	if err != nil {
		return swept, err
	}
	// A content removed frees the blobs it named, so contents are swept
	// until a pass removes none.
	cutoff := time.Now().Add(-grace)
	for {
		n, err := s.sweepPass(ctx,
			`SELECT c.Digest FROM Contents c
			 WHERE c.Written < @cutoff
			   AND NOT EXISTS (SELECT 1 FROM Entries e WHERE e.Content = c.Digest)
			   AND NOT EXISTS (SELECT 1 FROM Sidecars s WHERE s.Blob = c.Digest)`,
			map[string]any{"cutoff": cutoff},
			`SELECT c.Digest FROM Contents c
			 WHERE c.Digest IN UNNEST(@hs)
			   AND c.Written < @cutoff
			   AND NOT EXISTS (SELECT 1 FROM Entries e WHERE e.Content = c.Digest)
			   AND NOT EXISTS (SELECT 1 FROM Sidecars s WHERE s.Blob = c.Digest)`,
			func(row *gs.Row) ([]*gs.Mutation, error) {
				var h string
				if err := row.Columns(&h); err != nil {
					return nil, err
				}
				return []*gs.Mutation{
					gs.Delete("Contents", gs.Key{h}),
					gs.Delete("Sidecars", gs.Key{h}.AsPrefix()),
				}, nil
			})
		swept += n
		if err != nil || n == 0 {
			return swept, err
		}
	}
}

// sweepPass gathers candidates with find from a snapshot, then in
// batches re-checks them with recheck inside a read-write transaction
// and removes what is still unreferenced.
func (s *Store) sweepPass(ctx context.Context, find string, params map[string]any, recheck string, remove func(*gs.Row) ([]*gs.Mutation, error)) (int, error) {
	var candidates []string
	err := s.client.Single().Query(ctx, gs.Statement{SQL: find, Params: params}).Do(func(row *gs.Row) error {
		var h string
		if err := row.Columns(&h); err != nil {
			return err
		}
		candidates = append(candidates, h)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("spanner: sweep: %w", err)
	}
	sort.Strings(candidates)
	swept := 0
	for len(candidates) > 0 {
		batch := candidates[:min(sweepBatch, len(candidates))]
		candidates = candidates[len(batch):]
		p := map[string]any{"hs": batch}
		for k, v := range params {
			p[k] = v
		}
		var n int
		_, err := s.client.ReadWriteTransaction(ctx, func(ctx context.Context, txn *gs.ReadWriteTransaction) error {
			n = 0
			var ms []*gs.Mutation
			err := txn.Query(ctx, gs.Statement{SQL: recheck, Params: p}).Do(func(row *gs.Row) error {
				m, err := remove(row)
				if err != nil {
					return err
				}
				ms = append(ms, m...)
				n++
				return nil
			})
			if err != nil {
				return err
			}
			return txn.BufferWrite(ms)
		})
		if err != nil {
			return swept, fmt.Errorf("spanner: sweep: %w", err)
		}
		swept += n
	}
	return swept, nil
}

var _ agentsession.Store = (*Store)(nil)
