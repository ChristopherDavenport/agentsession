package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// importSession is RFC 0003's import: POST /v1/sessions/import with an
// RFC 0001 file as the body, which the server holds to RFC 0002's import
// checks and hands to its store's import, committed before it answers.
// ?record=true says the file is the only copy, so the server is the
// session's record. A sidecar session's blobs are uploaded first as
// media objects, and one the server cannot find for the caller is
// missing_object. A refused import admits nothing.
func (h *Handler) importSession(w http.ResponseWriter, r *http.Request) {
	im, ok := h.store.(agentsession.Importer)
	if !ok {
		h.writeProblem(w, problem(CodeUnsupported, "the store has no import", ""))
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		h.fail(w, problem(CodeBadRequest, "the body could not be read", err.Error()))
		return
	}
	if len(data) > maxBody {
		h.fail(w, problem(CodeBadRequest, "the file is larger than the server takes", ""))
		return
	}
	// The file is read for its header and what it names; the store's
	// import holds it to RFC 0002's checks, as a local import does.
	file, err := agentsession.Read(bytes.NewReader(data))
	if err != nil {
		h.fail(w, importProblem(err))
		return
	}
	hdr := file.Header()
	ctx := r.Context()
	who := h.principal(r)
	// Authorized as a create of the session is, with exchange scope: the
	// ID, and for a fork the origin, as a read, and the base reached
	// through a session the caller may read.
	if !h.may(ctx, ScopeExchange, hdr.ID) || (hdr.Base != "" && hdr.ParentSession != "" && !h.may(ctx, ScopeRead, hdr.ParentSession)) {
		h.fail(w, noSession())
		return
	}
	if hdr.Base != "" && h.authorize != nil && h.reader != nil {
		k := objKey{cas.SpaceEntries, hdr.Base}
		ok, err := h.reachable(ctx, who.Name, []objKey{k})
		if err != nil {
			h.fail(w, err)
			return
		}
		if !ok[k] && !file.Prefix(hdr.Base) {
			h.fail(w, problem(CodeNoEntry, "no such entry", ""))
			return
		}
	}
	if hdr.Media == agentsession.MediaSidecar && !h.holdsMedia() {
		h.fail(w, fmt.Errorf("%w: %s", agentsession.ErrNoBlobWriter, hdr.ID))
		return
	}
	// Every sidecar blob a line names, whatever the header's media, must
	// be one the caller reaches, as for an append: naming one is no way
	// to reach it.
	blobs, err := agentsession.SessionBlobs(file)
	if err != nil {
		h.fail(w, err)
		return
	}
	missing, err := h.unreachedMedia(ctx, who.Name, blobs)
	if err != nil {
		h.fail(w, err)
		return
	}
	if missing != nil {
		p := problem(CodeMissingObject, "the import names sidecar blobs the server cannot find", "")
		p.Missing = missing
		h.fail(w, p)
		return
	}
	st := h.state(hdr.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	// The store holds a fork to the media of an origin the caller may
	// read, and one it may not is as one it does not hold, so the answer
	// says nothing of another's session.
	sess, err := im.Import(h.readableCtx(ctx), bytes.NewReader(data), r.URL.Query().Get("record") == "true")
	if err != nil {
		h.fail(w, importProblem(err))
		return
	}
	info := sessionInfo{Header: sess.Header(), Head: headOut(sess.Leaf()), Length: sess.Len(), Incarnation: incarnation(sess.Header())}
	info.Mark = h.markOf(ctx, hdr.ID)
	w.Header().Set("Incarnation", info.Incarnation)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, info)
}

// importProblem is the problem an import is refused with: the store's,
// a line that fails a rule of the format being bad_entry with the rule
// as its reason, and a last line cut short or an unresolved migration
// bad_entry too.
func importProblem(err error) *Problem {
	p := problemOf(err)
	var tl *agentsession.TruncatedLine
	switch {
	case p.Code != CodeStoreFault:
	case errors.As(err, &tl):
		p = problem(CodeBadEntry, "the file's last line is cut short", err.Error())
		p.Reason = "truncated"
	case errors.Is(err, agentsession.ErrUnresolvedMigration):
		p = problem(CodeBadEntry, "the file is a migration that left an entry unresolved", err.Error())
	}
	if p.Code == CodeSynthetic && errors.Is(err, agentsession.ErrSyntheticMarker) {
		p.Reason = "synthetic_marker"
	}
	return p
}

// Import implements [agentsession.Importer] over RFC 0003's import: it
// posts the file, which the server holds to RFC 0002's import checks and
// commits before it answers, and returns the session as the server
// holds it, read back. A sidecar session's blobs must be on the server
// already, uploaded or held by a session the caller may read, or the
// import is refused with [ErrMissingObject], which matches
// [agentsession.ErrNoBlob] too; [Client.ImportMedia] uploads them first.
func (c *Client) Import(ctx context.Context, r io.Reader, asRecord bool) (*agentsession.Session, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return c.importFile(ctx, data, asRecord)
}

func (c *Client) importFile(ctx context.Context, data []byte, asRecord bool) (*agentsession.Session, error) {
	// A last line cut short is refused here, as a local import refuses
	// it, and by the server too.
	if file, err := agentsession.Read(bytes.NewReader(data)); err == nil && file.Truncated() != nil {
		return nil, fmt.Errorf("remote: import: %w", file.Truncated())
	}
	q := url.Values{}
	if asRecord {
		q.Set("record", "true")
	}
	resp, err := c.do(ctx, http.MethodPost, "/sessions/import", q, http.Header{"Content-Type": {"application/x-ndjson"}}, data)
	if errors.Is(err, ErrMissingObject) {
		// A sidecar blob the server cannot find for the caller.
		return nil, fmt.Errorf("remote: import: %w: %w", agentsession.ErrNoBlob, err)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var info sessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("remote: import: %w", err)
	}
	return c.Read(ctx, info.Header.ID)
}

// ImportMedia implements [agentsession.MediaImporter]: for a sidecar
// session it uploads each blob the items name from media, verified
// against its name, then imports the file. A blob media lacks is not
// uploaded; the server refuses the import with [ErrMissingObject] and
// [agentsession.ErrNoBlob] unless it holds the blob for the caller
// already, and nothing is imported. So
// over the wire a missing blob refuses the import, where a local store
// takes the session and reports the blob unresolved; the unresolved list
// returned here is always empty.
func (c *Client) ImportMedia(ctx context.Context, r io.Reader, media fs.FS, asRecord bool) (*agentsession.Session, []string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	file, err := agentsession.ReadForImport(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	if file.Header().Media == agentsession.MediaSidecar {
		blobs, err := agentsession.SessionBlobs(file)
		if err != nil {
			return nil, nil, err
		}
		for _, h := range blobs {
			b, err := agentsession.ReadSidecarBlob(media, h)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if err := c.PutObject(ctx, cas.SpaceMedia, h, b); err != nil {
				return nil, nil, err
			}
		}
	}
	sess, err := c.importFile(ctx, data, asRecord)
	return sess, nil, err
}

// CheckPush is RFC 0003's push pre-flight: it posts the session's header
// and its entries and prefix by hash to push?check, and returns the
// session-level refusal the push would meet, or nil, with nothing sent
// or stored but the check. It is advisory; the push is the authority.
func (c *Client) CheckPush(ctx context.Context, h agentsession.Header, entries, prefix []string) error {
	req := pushCheck{Header: h, Entries: entries, Prefix: prefix}
	if req.Entries == nil {
		req.Entries = []string{}
	}
	if req.Prefix == nil {
		req.Prefix = []string{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(h.ID)+"/push", url.Values{"check": {""}}, nil, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// pushCheck is a push pre-flight's body.
type pushCheck struct {
	Header  agentsession.Header `json:"header"`
	Entries []string            `json:"entries"`
	Prefix  []string            `json:"prefix"`
}

var (
	_ agentsession.Importer      = (*Client)(nil)
	_ agentsession.MediaImporter = (*Client)(nil)
)
