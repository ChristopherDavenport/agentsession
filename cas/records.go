package cas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
)

// logRecord is one line of a session's log: one record, as RFC 0002
// names them.
//
// A record ends in a CRC-32C of the bytes before it, so a record
// damaged after it was written is found rather than read as something
// else. A crash cuts a record short only at the end of the log, and the
// log's holder truncates the torn bytes before it writes again; a whole
// line holding no record that reads is damage, never a torn write. A
// record written before the checksum existed has none and is taken as
// it reads.
type logRecord struct {
	Op      string `json:"op"` // create, append, head, mark, sync, lost
	Session string `json:"session"`
	Entry   string `json:"entry,omitempty"`
	Head    string `json:"head,omitempty"`
	Seq     int    `json:"seq,omitempty"`
	// Base is a created session's base.
	Base string `json:"base,omitempty"`
	// Mark is the record mark a mark record sets.
	Mark string `json:"mark,omitempty"`
	// Size is the bytes of the entry's envelope and content.
	Size int64 `json:"size,omitempty"`
	// Lazy is set on an append acknowledged before it was durable. Its
	// objects were not fsynced ahead of the record, so after a crash a
	// lazy record whose objects are missing is an append that was lost,
	// not damage.
	Lazy bool `json:"lazy,omitempty"`

	checked bool // it carried a checksum that matched
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

const crcMember = `,"crc":"`

// encode renders the record as one log line with its checksum.
func (r logRecord) encode() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	body := data[:len(data)-1] // without the closing brace
	sum := crc32.Checksum(body, crcTable)
	line := make([]byte, 0, len(data)+20)
	line = append(line, body...)
	line = append(line, crcMember...)
	line = strconv.AppendUint(line, uint64(sum), 16)
	line = append(line, '"', '}', '\n')
	return line, nil
}

// decodeRecord parses one record. torn is set for bytes a crashed write
// cut short, which recovery skips; any other failure is damage.
func decodeRecord(seg []byte) (rec logRecord, torn bool, err error) {
	seg = bytes.TrimRight(seg, "\n")
	if len(seg) == 0 || seg[len(seg)-1] != '}' {
		return rec, true, errors.New("cut short")
	}
	if k := bytes.LastIndex(seg, []byte(crcMember)); k >= 0 {
		tail := seg[k+len(crcMember):]
		if len(tail) < 3 || tail[len(tail)-2] != '"' {
			return rec, false, errors.New("malformed checksum")
		}
		want, perr := strconv.ParseUint(string(tail[:len(tail)-2]), 16, 32)
		if perr != nil {
			return rec, false, errors.New("malformed checksum")
		}
		if crc32.Checksum(seg[:k], crcTable) != uint32(want) {
			return rec, false, errors.New("checksum mismatch")
		}
	}
	if r, ok := parseRecord(seg); ok {
		rec = r
	} else if err := json.Unmarshal(seg, &rec); err != nil {
		return rec, false, err
	}
	rec.checked = bytes.Contains(seg, []byte(crcMember))
	if rec.Op == "" || rec.Session == "" {
		return rec, false, errors.New("record names no operation or session")
	}
	return rec, false, nil
}

// parseRecord reads a record in the shape encode writes it: one flat
// object of known lower-case members, strings without escapes, whole
// numbers and true. It reports false for anything else, which the
// caller hands to encoding/json, so a record read here reads as that
// would read it; replay is mostly this, and the decoder's reflection
// was most of what opening a store cost.
func parseRecord(seg []byte) (logRecord, bool) {
	var r logRecord
	if len(seg) < 2 || seg[0] != '{' || seg[len(seg)-1] != '}' {
		return r, false
	}
	i := 1
	str := func() (string, bool) {
		if i >= len(seg) || seg[i] != '"' {
			return "", false
		}
		start := i + 1
		for j := start; j < len(seg); j++ {
			switch c := seg[j]; {
			case c == '"':
				i = j + 1
				return string(seg[start:j]), true
			case c == '\\' || c < 0x20 || c >= 0x80:
				return "", false
			}
		}
		return "", false
	}
	num := func() (int64, bool) {
		start, neg := i, false
		if i < len(seg) && seg[i] == '-' {
			neg = true
			i++
		}
		digits := i
		var n int64
		for i < len(seg) && seg[i] >= '0' && seg[i] <= '9' {
			if i-digits >= 18 {
				return 0, false
			}
			n = n*10 + int64(seg[i]-'0')
			i++
		}
		if i == digits || (seg[digits] == '0' && i-digits > 1) || i-start == 0 {
			return 0, false
		}
		if neg {
			n = -n
		}
		return n, true
	}
	if seg[i] == '}' {
		return r, i == len(seg)-1
	}
	for {
		key, ok := str()
		if !ok || i >= len(seg) || seg[i] != ':' {
			return r, false
		}
		i++
		switch key {
		case "op", "session", "entry", "head", "base", "mark", "crc":
			v, ok := str()
			if !ok {
				return r, false
			}
			switch key {
			case "op":
				r.Op = v
			case "session":
				r.Session = v
			case "entry":
				r.Entry = v
			case "head":
				r.Head = v
			case "base":
				r.Base = v
			case "mark":
				r.Mark = v
			}
		case "seq", "size":
			n, ok := num()
			if !ok {
				return r, false
			}
			if key == "seq" {
				r.Seq = int(n)
			} else {
				r.Size = n
			}
		case "lazy":
			if !bytes.HasPrefix(seg[i:], []byte("true")) {
				return r, false
			}
			r.Lazy = true
			i += len("true")
		default:
			return r, false
		}
		if i >= len(seg) {
			return r, false
		}
		switch seg[i] {
		case ',':
			i++
		case '}':
			return r, i == len(seg)-1
		default:
			return r, false
		}
	}
}

// LogDamage is a record of a session's log, or of the store-wide
// journal a store migrates from, that was written whole and no longer
// reads: a flipped bit, or a restore that put back part of a file.
// Recovery never takes it for a torn write.
type LogDamage struct {
	Line   int   // 1-based
	Offset int64 // the byte offset of the line
	Err    error
}

func (d LogDamage) Error() string {
	return fmt.Sprintf("cas: log line %d (offset %d) is damaged: %v", d.Line, d.Offset, d.Err)
}

// errNewline is damage that lost nothing: a record's newline damaged
// into another byte, with the record before it and the one after it
// both read whole.
var errNewline = errors.New("a record's newline is damaged")

// recordOpening begins every record a line holds.
var recordOpening = []byte(`{"op":"`)

// decodeLine reads the records of one log line. A line normally holds
// one. In the store-wide journal a store migrates from, a crash could
// cut a record short and the next record land on the same line after
// the torn bytes, so bytes ahead of the first whole record are skipped
// as torn. Every other piece that does not read is damage: a record
// whose newline was damaged into another byte is read with the record
// it ran into, and a whole line holding no record that reads is
// reported.
func decodeLine(line []byte) ([]logRecord, error) {
	var starts []int
	for i := 0; ; {
		k := bytes.Index(line[i:], recordOpening)
		if k < 0 {
			break
		}
		starts = append(starts, i+k)
		i += k + 1
	}
	if len(starts) == 0 {
		return nil, errors.New("no record")
	}
	var recs []logRecord
	var damage error
	for n, st := range starts {
		end := len(line)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		seg := line[st:end]
		rec, torn, err := decodeRecord(seg)
		if err != nil && len(seg) > 1 {
			// A record followed by one damaged byte where its newline was:
			// read, and reported.
			if r2, _, err2 := decodeRecord(seg[:len(seg)-1]); err2 == nil {
				recs = append(recs, r2)
				if damage == nil {
					damage = errNewline
				}
				continue
			}
		}
		if err != nil {
			if n+1 < len(starts) && len(recs) == 0 && torn {
				continue // torn bytes a later record landed after
			}
			if damage == nil {
				damage = err
			}
			continue
		}
		recs = append(recs, rec)
	}
	if starts[0] > 0 && len(recs) == 0 && damage == nil {
		damage = errors.New("no record")
	}
	return recs, damage
}
