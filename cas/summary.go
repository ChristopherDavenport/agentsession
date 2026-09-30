package cas

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ChristopherDavenport/agentsession"
)

// A session's summary — its size, and the name and successor a listing
// with WithNames or Current asks for — costs a read of its log, and the
// name a decode of every entry, so a listing of a large store computed
// afresh reads the whole store. The summary is kept in the session's
// directory beside the log it was computed from, with the log's size
// and modification time, and a listing uses it while the log is
// unchanged. Like HEAD, it is an index: written
// without an fsync, rewritten when it does not match, and never trusted
// for anything but a listing.

// summaryName is the file a session's summary is kept in.
const summaryName = "summary"

type summaryFile struct {
	LogSize      int64  `json:"log_size"`
	LogModified  int64  `json:"log_modified"`
	Size         int64  `json:"size"`
	Meta         bool   `json:"meta"`
	Name         string `json:"name,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
}

// logStamp is the log's size and modification time, which a summary
// is kept against; a session with no log has the zero stamp.
func logStamp(dir string) (size, modified int64) {
	info, err := os.Stat(filepath.Join(dir, "log"))
	if err != nil {
		return 0, 0
	}
	return info.Size(), info.ModTime().UnixNano()
}

// cachedSummary returns the session's kept summary when it was computed
// from the log as it stands and holds the name and successor when meta
// asks for them.
func cachedSummary(dir string, meta bool) (summaryFile, bool) {
	var c summaryFile
	data, err := os.ReadFile(filepath.Join(dir, summaryName))
	if err != nil || json.Unmarshal(data, &c) != nil {
		return c, false
	}
	size, modified := logStamp(dir)
	if c.LogSize != size || c.LogModified != modified || (meta && !c.Meta) {
		return c, false
	}
	return c, true
}

// keepSummary writes the summary against the log's stamp, taken before
// the summary was computed, so a log that changed meanwhile leaves one
// that does not match.
func keepSummary(dir string, size, modified int64, sum agentsession.Summary, meta bool) {
	c := summaryFile{LogSize: size, LogModified: modified, Size: sum.Size, Meta: meta}
	if meta {
		c.Name, c.SupersededBy = sum.Name, sum.SupersededBy
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = writeIndex(filepath.Join(dir, summaryName), append(data, '\n'))
}

// summarizeHeld computes and keeps the summary of a session this store
// holds, as it releases it: the name and successor come from the
// session in memory, so nothing is decoded. It runs under mu.
func (s *Store) summarizeHeld(id string, h *handle) {
	if s.readOnly {
		return
	}
	size, modified := logStamp(h.dir)
	v, err := s.reconcile(id, h.dir)
	if err != nil || !v.exists || len(v.adopt) > 0 {
		return
	}
	sum, err := s.summarize(id, h.dir, v, false)
	if err != nil {
		return
	}
	for _, hash := range v.log {
		e, ok := h.session.Entry(hash)
		if !ok {
			return // the session in memory is not the log's; leave it
		}
		switch e := e.(type) {
		case *agentsession.InfoEntry:
			if e.Name != "" {
				sum.Name = e.Name
			}
		case *agentsession.LinkEntry:
			if e.Rel == agentsession.RelContinuedIn && e.Session != "" {
				sum.SupersededBy = e.Session
			}
		}
	}
	keepSummary(h.dir, size, modified, sum, true)
}
