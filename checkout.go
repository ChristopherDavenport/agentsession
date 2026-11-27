package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// Checkout is one checkout an env entry lists (RFC 0001, env, since
// 0.22): a tree the session's tools acted in, beside or instead of the
// one at the entry's CWD, such as a worktree the session made or a
// repository below a CWD that is not one.
//
// What a writer records beyond these members goes in a namespaced
// member, set with [Checkout.SetMember], and survives a rewrite as
// written. Replacing a checkout whole drops such members; a caller
// changing a field edits the Checkout it has, or a copy of it.
type Checkout struct {
	// Path is the checkout's top directory: relative to the entry's
	// CWD, as slash-separated segments none of which is empty, "." or
	// "..", or "." for the CWD itself, when the checkout is under it;
	// absolute otherwise. It is required, and unique in a list.
	Path string `json:"path"`
	// Repo names the repository the checkout belongs to, in the form
	// Path takes, so checkouts of one repository can be grouped: for
	// git, the common git directory every worktree shares. Empty when
	// the writer names none.
	Repo string `json:"repo,omitempty"`
	// VCS is the checkout's state, with the meanings the entry's own
	// VCS has. It is nil for a checkout the writer knows of but could
	// not read. The checkout at "." carries the entry's VCS.
	VCS *VCS `json:"vcs,omitempty"`
	// Branch is the branch the checkout is on, in the system's short
	// form (main, not refs/heads/main); empty when it is on none, as a
	// detached HEAD is.
	Branch string `json:"branch,omitempty"`
	// Upstream is the branch's upstream, when the writer read one.
	Upstream *Upstream `json:"upstream,omitempty"`
	// Unknown holds the members of the checkout the format does not
	// define, encoded inline beside the others. It is nil when there are
	// none.
	Unknown map[string]json.RawMessage `json:"-"`
}

// Upstream is the upstream of a checkout's branch: its name as the
// checkout names it, such as origin/main, and the revision it was on,
// which follows the rules of [VCS].Revision. A push or a fetch shows as
// its revision moving. A remote's URL is never recorded in a checkout.
type Upstream struct {
	Name     string `json:"name,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Unknown holds the members the format does not define, encoded
	// inline. It is nil when there are none.
	Unknown map[string]json.RawMessage `json:"-"`
}

var (
	checkoutMembers = map[string]bool{"path": true, "repo": true, "vcs": true, "branch": true, "upstream": true}
	upstreamMembers = map[string]bool{"name": true, "revision": true}
)

// SetMember sets a member of the checkout the format does not define,
// which should be namespaced, such as "acme:tree".
func (c *Checkout) SetMember(key string, val any) error {
	return setUndefined(&c.Unknown, checkoutMembers, "checkout", key, val)
}

// SetMember sets a member of the upstream the format does not define.
func (u *Upstream) SetMember(key string, val any) error {
	return setUndefined(&u.Unknown, upstreamMembers, "upstream", key, val)
}

func setUndefined(dst *map[string]json.RawMessage, defined map[string]bool, what, key string, val any) error {
	if defined[key] {
		return fmt.Errorf("agentsession: %s member %q is defined; set the field", what, key)
	}
	data, err := jsonx.MarshalNoEscape(val)
	if err != nil {
		return fmt.Errorf("agentsession: %s member %q: %w", what, key, err)
	}
	if *dst == nil {
		*dst = make(map[string]json.RawMessage)
	}
	(*dst)[key] = data
	return nil
}

// MarshalJSON emits the defined members and the unknown ones as one
// object.
func (c Checkout) MarshalJSON() ([]byte, error) {
	for key := range c.Unknown {
		if checkoutMembers[key] {
			return nil, fmt.Errorf("agentsession: checkout has %s both typed and unknown", key)
		}
	}
	type plain Checkout
	b, err := jsonx.MarshalNoEscape(plain(c))
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(b, nil, c.Unknown), nil
}

// UnmarshalJSON takes the defined members, spelled exactly and each in
// the form the format gives it, and keeps every other member in
// Unknown. A defined member in another form, null included, is an
// error, so that the decoder leaves a list holding it as written.
func (c *Checkout) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*c = Checkout{}
	for key, raw := range all {
		var err error
		switch key {
		case "path":
			err = stringMember(raw, &c.Path)
		case "repo":
			err = stringMember(raw, &c.Repo)
		case "branch":
			err = stringMember(raw, &c.Branch)
		case "vcs":
			if !isObject(raw) {
				err = errors.New("not an object")
				break
			}
			c.VCS = &VCS{}
			err = json.Unmarshal(raw, c.VCS)
		case "upstream":
			if !isObject(raw) {
				err = errors.New("not an object")
				break
			}
			c.Upstream = &Upstream{}
			err = json.Unmarshal(raw, c.Upstream)
		default:
			if c.Unknown == nil {
				c.Unknown = make(map[string]json.RawMessage)
			}
			c.Unknown[key] = raw
		}
		if err != nil {
			return fmt.Errorf("checkout %s: %w", key, err)
		}
	}
	return nil
}

// MarshalJSON emits the defined members and the unknown ones as one
// object.
func (u Upstream) MarshalJSON() ([]byte, error) {
	for key := range u.Unknown {
		if upstreamMembers[key] {
			return nil, fmt.Errorf("agentsession: upstream has %s both typed and unknown", key)
		}
	}
	type plain Upstream
	b, err := jsonx.MarshalNoEscape(plain(u))
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(b, nil, u.Unknown), nil
}

// UnmarshalJSON takes name and revision as strings and keeps every
// other member in Unknown.
func (u *Upstream) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*u = Upstream{}
	for key, raw := range all {
		var err error
		switch key {
		case "name":
			err = stringMember(raw, &u.Name)
		case "revision":
			err = stringMember(raw, &u.Revision)
		default:
			if u.Unknown == nil {
				u.Unknown = make(map[string]json.RawMessage)
			}
			u.Unknown[key] = raw
		}
		if err != nil {
			return fmt.Errorf("upstream %s: %w", key, err)
		}
	}
	return nil
}

// stringMember decodes raw into dst only when it is a JSON string.
func stringMember(raw json.RawMessage, dst *string) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '"' {
		return errors.New("not a string")
	}
	return json.Unmarshal(raw, dst)
}

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

// ValidCheckoutPath reports whether p is a checkout's path or repo in
// the form RFC 0001 gives it: "." or a relative name of slash-separated
// segments none of which is empty, "." or "..", or an absolute path.
func ValidCheckoutPath(p string) bool {
	if p == "" {
		return false
	}
	if absolutePath(p) {
		return true
	}
	return fs.ValidPath(p) && !strings.Contains(p, `\`)
}

// absolutePath reports a POSIX absolute path, a Windows drive path
// (C:\ or C:/) or a UNC path, whatever system this runs on, since a
// session written on one is read on another.
func absolutePath(p string) bool {
	if path.IsAbs(p) || strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z')
}

// validateCheckouts checks a list of checkouts against the forms RFC
// 0001 gives them: every path present and in form, every repo empty or
// in form, and no path listed twice.
func validateCheckouts(list []Checkout) error {
	seen := make(map[string]bool, len(list))
	for i, c := range list {
		if !ValidCheckoutPath(c.Path) {
			return fmt.Errorf("agentsession: checkout %d: path %q is not \".\", a relative name or an absolute path", i, c.Path)
		}
		if c.Repo != "" && !ValidCheckoutPath(c.Repo) {
			return fmt.Errorf("agentsession: checkout %s: repo %q is not \".\", a relative name or an absolute path", c.Path, c.Repo)
		}
		if seen[c.Path] {
			return fmt.Errorf("agentsession: checkout %s is listed twice", c.Path)
		}
		seen[c.Path] = true
	}
	return nil
}

// checkoutsForm is the decoder's test of a checkouts member: it takes
// the list only when validateCheckouts would, and leaves any other as
// written.
func checkoutsForm(list []Checkout) bool { return validateCheckouts(list) == nil }

// Checkout returns the checkout the entry lists at path, or nil.
func (e *EnvEntry) Checkout(path string) *Checkout {
	for i := range e.Checkouts {
		if e.Checkouts[i].Path == path {
			return &e.Checkouts[i]
		}
	}
	return nil
}

// ErrCheckoutVCS is reported by [Session.VerifyRecords], for an env
// entry in a file declaring 0.22 or later, and by an append, when the
// checkout at "." carries a vcs other than the entry's own, which RFC
// 0001 forbids a writer: a reader reads the entry's vcs for the tree
// at its CWD.
var ErrCheckoutVCS = errors.New("agentsession: the checkout at . carries a vcs other than the env entry's")

// checkoutVCSAgrees reports whether the checkout at "." carries the
// entry's VCS, compared in canonical form, absent equal only to
// absent; an entry with no checkout at "." agrees.
func (e *EnvEntry) checkoutVCSAgrees() bool {
	c := e.Checkout(".")
	return c == nil || sameVCS(c.VCS, e.VCS)
}

func sameVCS(a, b *VCS) bool {
	if a == nil || b == nil {
		return a == b
	}
	ca, err1 := canonicalJSON(*a)
	cb, err2 := canonicalJSON(*b)
	return err1 == nil && err2 == nil && bytes.Equal(ca, cb)
}

// CheckoutChangeKind says what moved in a checkout between two env
// entries.
type CheckoutChangeKind string

// The changes [CheckoutChanges] names.
const (
	// CheckoutAdded is a checkout listed after and not before: one made,
	// or first worked in.
	CheckoutAdded CheckoutChangeKind = "added"
	// CheckoutRemoved is one listed before and not after: removed, or no
	// longer followed.
	CheckoutRemoved CheckoutChangeKind = "removed"
	// CheckoutRevision is a new revision: a commit or a reset when the
	// branch is the same.
	CheckoutRevision CheckoutChangeKind = "revision"
	// CheckoutBranch is a new branch, or a move onto or off one: a
	// switch.
	CheckoutBranch CheckoutChangeKind = "branch"
	// CheckoutUpstream is an upstream that appeared, went or moved: a
	// push or a fetch when its revision moves.
	CheckoutUpstream CheckoutChangeKind = "upstream"
	// CheckoutDirty is the tree becoming dirty or clean.
	CheckoutDirty CheckoutChangeKind = "dirty"
	// CheckoutOther is any other difference: the repo, the vcs system,
	// or a member the format does not define, compared in canonical
	// form.
	CheckoutOther CheckoutChangeKind = "other"
)

// CheckoutChange is one move [CheckoutChanges] found. Before and After
// are the checkout as each entry lists it; Before is nil for an added
// checkout and After for a removed one.
type CheckoutChange struct {
	Path          string
	Kind          CheckoutChangeKind
	Before, After *Checkout
}

// CheckoutChanges names what moved in the checkouts between two env
// entries, checkout by path: those added, those removed, and for one
// in both, each of its revision, branch, upstream, dirty and anything
// else that differs, in that order. Added and changed checkouts come in
// after's order, removed ones in before's, after the rest. A nil entry,
// or one with no checkouts member, lists none, so the changes from it
// are every checkout of the other added (or removed). Folding it over a
// path's env entries in order follows the session's checkouts.
func CheckoutChanges(before, after *EnvEntry) []CheckoutChange {
	var b, a []Checkout
	if before != nil {
		b = before.Checkouts
	}
	if after != nil {
		a = after.Checkouts
	}
	old := make(map[string]*Checkout, len(b))
	for i := range b {
		old[b[i].Path] = &b[i]
	}
	var out []CheckoutChange
	kept := make(map[string]bool, len(a))
	for i := range a {
		n := &a[i]
		o, ok := old[n.Path]
		if !ok {
			out = append(out, CheckoutChange{Path: n.Path, Kind: CheckoutAdded, After: n})
			continue
		}
		kept[n.Path] = true
		for _, k := range checkoutDiff(o, n) {
			out = append(out, CheckoutChange{Path: n.Path, Kind: k, Before: o, After: n})
		}
	}
	for i := range b {
		if !kept[b[i].Path] {
			out = append(out, CheckoutChange{Path: b[i].Path, Kind: CheckoutRemoved, Before: &b[i]})
		}
	}
	return out
}

// checkoutDiff lists the kinds of change between two listings of one
// checkout.
func checkoutDiff(o, n *Checkout) []CheckoutChangeKind {
	var out []CheckoutChangeKind
	var ov, nv VCS
	if o.VCS != nil {
		ov = *o.VCS
	}
	if n.VCS != nil {
		nv = *n.VCS
	}
	if ov.Revision != nv.Revision {
		out = append(out, CheckoutRevision)
	}
	if o.Branch != n.Branch {
		out = append(out, CheckoutBranch)
	}
	if !sameEncoded(o.Upstream, n.Upstream) {
		out = append(out, CheckoutUpstream)
	}
	if ov.Dirty != nv.Dirty {
		out = append(out, CheckoutDirty)
	}
	// What is left: the repo, the vcs's system and members the format
	// does not define, whether a vcs is there at all, and the
	// checkout's own undefined members.
	ro, rn := *o, *n
	ro.Branch, rn.Branch = "", ""
	ro.Upstream, rn.Upstream = nil, nil
	if ro.VCS != nil {
		v := *ro.VCS
		v.Revision, v.Dirty = "", false
		ro.VCS = &v
	}
	if rn.VCS != nil {
		v := *rn.VCS
		v.Revision, v.Dirty = "", false
		rn.VCS = &v
	}
	if !sameEncoded(&ro, &rn) {
		out = append(out, CheckoutOther)
	}
	return out
}

// sameEncoded compares two values in canonical form, nil equal only to
// nil.
func sameEncoded[T any, P interface {
	*T
	json.Marshaler
}](a, b P) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ca, err1 := canonicalJSON(a)
	cb, err2 := canonicalJSON(b)
	return err1 == nil && err2 == nil && bytes.Equal(ca, cb)
}
