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
// nil; other errors are returned.
func (e *EnvEntry) SetGit(dir string, dirty bool) error {
	rev, err := GitRevision(dir)
	if err != nil {
		if errors.Is(err, ErrNotGit) {
			return nil
		}
		return err
	}
	e.VCS = &VCS{System: "git", Revision: rev, Dirty: dirty}
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

// NewLabelEntry builds a label on target; an empty label clears.
func NewLabelEntry(target, label string) *LabelEntry {
	e := &LabelEntry{Target: target}
	if label != "" {
		e.Label = &label
	}
	return e
}
