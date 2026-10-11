package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// objectCache keeps the objects a client read, by space and hash. An
// object never changes under its hash, so nothing in it goes stale.
type objectCache struct {
	objects map[objKey][]byte
}

// Object reads an object the server holds and the caller may read: an
// envelope (cas.SpaceEntries), a content (cas.SpaceContents) or a media
// blob (cas.SpaceMedia). The bytes are checked against the hash, and
// kept, so each object is fetched once.
func (c *Client) Object(ctx context.Context, space, hash string) ([]byte, error) {
	k := objKey{space, hash}
	c.mu.Lock()
	data, ok := c.cache.objects[k]
	c.mu.Unlock()
	if ok {
		return data, nil
	}
	if !validSpace(space) || !agentsession.ValidHash(hash) {
		return nil, fmt.Errorf("%w: %s %q", cas.ErrNoObject, space, hash)
	}
	resp, err := c.get(ctx, "/objects/"+space+"/"+hash, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("remote: object %s: %w", hash, err)
	}
	if agentsession.HashBytes(data) != hash {
		return nil, fmt.Errorf("remote: object %s: %w", hash, cas.ErrBadObject)
	}
	c.mu.Lock()
	if c.cache.objects == nil {
		c.cache.objects = map[objKey][]byte{}
	}
	c.cache.objects[k] = data
	c.mu.Unlock()
	return data, nil
}

// Missing asks which of the objects the server lacks for this caller:
// every one no session the caller may read holds, and that the caller
// did not upload.
func (c *Client) Missing(ctx context.Context, o Objects) (Objects, error) {
	body, err := json.Marshal(o)
	if err != nil {
		return Objects{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/objects/missing", nil, nil, body)
	if err != nil {
		return Objects{}, err
	}
	defer resp.Body.Close()
	var out Objects
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Objects{}, fmt.Errorf("remote: missing: %w", err)
	}
	return out, nil
}

// PutObject uploads an object a push will name.
func (c *Client) PutObject(ctx context.Context, space, hash string, data []byte) error {
	resp, err := c.do(ctx, http.MethodPut, "/objects/"+space+"/"+hash, nil, http.Header{"Content-Type": {"application/octet-stream"}}, data)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// DeclareRecord makes the server the record for a session it holds as a
// mirror, RFC 0002's declare-record, when its head is head, the one the
// caller fetched; basis is "record_deleted" or "sole_copy". It is how a
// replacement takes over a session whose local-first writer died: it
// declares the record at the server, then writes under a lease.
func (c *Client) DeclareRecord(ctx context.Context, id, basis, head string) error {
	body, err := json.Marshal(recordRequest{Basis: basis, Head: headOut(head)})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/record", nil, nil, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// HandOver makes the server a mirror of a session it is the record for,
// when its head is head: the server's side of handing the session to a
// client store, which has fetched it and then declares the record in its
// own store. It is a write, under the client's lease, which ends with
// it.
func (c *Client) HandOver(ctx context.Context, id, head string) error {
	hd, err := c.hold(ctx, id)
	if err != nil {
		return err
	}
	defer hd.mu.Unlock()
	header, err := c.fence(hd)
	if err != nil {
		return err
	}
	body, err := json.Marshal(handoverRequest{To: "client", Head: headOut(head)})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/handover", nil, header, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	c.drop(hd)
	return nil
}

// Push sends a session the local store is the record for to the server,
// RFC 0002's push over RFC 0003: it asks which objects of the session's
// closure the server lacks for this caller, uploads those, and posts the
// push, which the server admits as a mirror, or as the record for a
// handover. It is the helper of a local-first writer, which records into
// its own store with no network in its loop and pushes at commits, on a
// timer and at shutdown. opts are as for cas.Store.Push, and the result
// is what the server did, as Push between two stores reports it. A
// handover ends with the local store a mirror, once the server has
// committed its mark; a failure between the two leaves two records,
// never none.
func Push(ctx context.Context, local *cas.Store, c *Client, id string, opts cas.PushOptions) (cas.Exchange, error) {
	targets := make([]agentsession.RefTarget, len(opts.Refs))
	for i, r := range opts.Refs {
		t, err := local.ResolveRef(ctx, r.Name)
		if err != nil {
			return cas.Exchange{}, fmt.Errorf("remote: push: ref %s: %w", r.Name, err)
		}
		if t.Session != id {
			return cas.Exchange{}, fmt.Errorf("remote: push: ref %s points at %s, not at %s: %w", r.Name, t.Session, id, ErrBadRefTarget)
		}
		targets[i] = t
	}
	cl, err := local.Closure(ctx, id)
	if err != nil {
		return cas.Exchange{}, err
	}
	if cl.Mark != cas.MarkRecord {
		return cas.Exchange{}, fmt.Errorf("%w: the local store holds %s as a mirror", cas.ErrNotRecord, id)
	}
	objects := map[objKey][]byte{}
	var all Objects
	add := func(space, hash string, data []byte) {
		k := objKey{space, hash}
		if _, ok := objects[k]; ok {
			return
		}
		objects[k] = data
		all.add(space, hash)
	}
	req := pushRequest{Header: cl.Header, Entries: []string{}, Prefix: []string{}, Refs: []PushRef{},
		Head: pushHeadStep{Expected: headOut(opts.Expected), To: headOut(cl.Head), Force: opts.Force}, Mark: cas.MarkMirror}
	if opts.Handover {
		req.Mark = cas.MarkRecord
	}
	for i, es := range [][]agentsession.Entry{cl.Prefix, cl.Own} {
		for _, e := range es {
			env, body, err := cas.SplitEntry(e)
			if err != nil {
				return cas.Exchange{}, err
			}
			add(cas.SpaceEntries, e.Base().ID, env)
			add(cas.SpaceContents, e.Base().ContentHash(), body)
			if i == 0 {
				req.Prefix = append(req.Prefix, e.Base().ID)
			} else {
				req.Entries = append(req.Entries, e.Base().ID)
			}
		}
	}
	for h, data := range cl.Blobs {
		add(cas.SpaceMedia, h, data)
	}
	for i, r := range opts.Refs {
		req.Refs = append(req.Refs, PushRef{Name: r.Name, Expected: targetOut(r.Expected), Next: targetOut(targets[i])})
	}
	lacks, err := c.Missing(ctx, all)
	if err != nil {
		return cas.Exchange{}, err
	}
	var upErr error
	lacks.each(func(space, hash string) {
		if upErr != nil {
			return
		}
		data, ok := objects[objKey{space, hash}]
		if !ok {
			upErr = fmt.Errorf("remote: push: the server asked for %s %s, which the push does not name", space, hash)
			return
		}
		upErr = c.PutObject(ctx, space, hash, data)
	})
	if upErr != nil {
		return cas.Exchange{}, upErr
	}
	body, err := json.Marshal(req)
	if err != nil {
		return cas.Exchange{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/push", nil, nil, body)
	if err != nil {
		var p *Problem
		if errors.As(err, &p) && p.Push != nil {
			return exchangeOf(*p.Push, targets), err
		}
		return cas.Exchange{}, err
	}
	defer resp.Body.Close()
	var res PushResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return cas.Exchange{}, fmt.Errorf("remote: push %s: %w", id, err)
	}
	x := exchangeOf(res, targets)
	if !opts.Handover {
		return x, nil
	}
	// The server is the record now, committed; clear the mark here.
	if err := local.HandOver(ctx, id, cl.Head); err != nil {
		return x, fmt.Errorf("remote: handover: the server is the record, and the local store could not clear its mark: %w", err)
	}
	x.Handover = true
	return x, nil
}

// exchangeOf is a push's result as cas.Exchange reports one, its refs'
// targets those the push carried.
func exchangeOf(r PushResult, targets []agentsession.RefTarget) cas.Exchange {
	x := cas.Exchange{Created: r.Created, Admitted: r.Admitted, Head: headIn(r.Head.Current), HeadMoved: r.Head.Moved, Forced: r.Head.Forced, Why: r.Head.Why}
	for i, rr := range r.Refs {
		out := cas.RefResult{Name: rr.Name, Moved: rr.Moved, Current: rr.Current.lib(), Why: rr.Why}
		if i < len(targets) {
			out.Target = targets[i]
		}
		x.Refs = append(x.Refs, out)
	}
	return x
}
