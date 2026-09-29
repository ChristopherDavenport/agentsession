package cas

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A pack holds many objects in one file, as git's packfiles do, so a
// store of small entries does not pay a filesystem block and two
// fsyncs for each. A pack is immutable and named by its checksum:
//
//	pack-<hex>.pack  "CASPACK\x00", version, count, then per object:
//	                 space, 32-byte digest, uvarint length, bytes;
//	                 then the SHA-256 of everything before it
//	pack-<hex>.idx   "CASIDX\x00\x00", version, count, then per object,
//	                 sorted by space and digest: space, digest, offset,
//	                 length; then the pack's checksum and the SHA-256 of
//	                 everything before it
//
// The index is written after the pack and is what makes the pack
// visible: a pack without one is an interrupted write, which the sweep
// removes once it is old. Objects are stored as they are, since an
// object's name is the hash of its bytes and a reader checks it.
const (
	packMagic  = "CASPACK\x00"
	idxMagic   = "CASIDX\x00\x00"
	packFormat = 1
	idxRecord  = 1 + sha256.Size + 8 + 8
)

// Object spaces. RFC 0002 keeps envelopes apart from bodies; a pack
// holds both and tags each object with its space.
type space byte

const (
	spaceEntries  space = 'e'
	spaceContents space = 'c'
)

func (sp space) String() string {
	if sp == spaceEntries {
		return "entry"
	}
	return "content"
}

// packObject is one object to put in a pack.
type packObject struct {
	sp   space
	hash string
	data []byte
}

// pack is an open pack: its index in memory, its file open for reads.
type pack struct {
	name string // pack-<hex>, without extension
	path string // the .pack file
	idx  []byte // the index records, sorted
	f    *os.File
}

func digestOf(hash string) ([]byte, error) {
	d, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	if err != nil || len(d) != sha256.Size || !strings.HasPrefix(hash, "sha256:") {
		return nil, fmt.Errorf("%w: hash %q", ErrBadName, hash)
	}
	return d, nil
}

// find returns the offset and length of an object in the pack.
func (p *pack) find(sp space, digest []byte) (int64, int64, bool) {
	n := len(p.idx) / idxRecord
	key := append([]byte{byte(sp)}, digest...)
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(p.idx[i*idxRecord:i*idxRecord+1+sha256.Size], key) >= 0
	})
	if i == n {
		return 0, 0, false
	}
	rec := p.idx[i*idxRecord : (i+1)*idxRecord]
	if !bytes.Equal(rec[:1+sha256.Size], key) {
		return 0, 0, false
	}
	off := int64(binary.BigEndian.Uint64(rec[1+sha256.Size:]))
	length := int64(binary.BigEndian.Uint64(rec[1+sha256.Size+8:]))
	return off, length, true
}

// read returns an object's bytes from the pack.
func (p *pack) read(off, length int64) ([]byte, error) {
	buf := make([]byte, length)
	if _, err := p.f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}

// each calls fn for every object the index lists.
func (p *pack) each(fn func(sp space, hash string, off, length int64) error) error {
	for i := 0; i+idxRecord <= len(p.idx); i += idxRecord {
		rec := p.idx[i : i+idxRecord]
		hash := "sha256:" + hex.EncodeToString(rec[1:1+sha256.Size])
		off := int64(binary.BigEndian.Uint64(rec[1+sha256.Size:]))
		length := int64(binary.BigEndian.Uint64(rec[1+sha256.Size+8:]))
		if err := fn(space(rec[0]), hash, off, length); err != nil {
			return err
		}
	}
	return nil
}

func (p *pack) close() error {
	if p.f == nil {
		return nil
	}
	err := p.f.Close()
	p.f = nil
	return err
}

// openPack loads a pack's index and opens the pack, checking the index's
// own checksum and that it names this pack.
func openPack(dir, name string) (*pack, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name+".idx"))
	if err != nil {
		return nil, err
	}
	head := len(idxMagic) + 8
	if len(raw) < head+2*sha256.Size || string(raw[:len(idxMagic)]) != idxMagic {
		return nil, fmt.Errorf("%w: %s.idx is not an index", ErrCorrupt, name)
	}
	body := raw[:len(raw)-sha256.Size]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], raw[len(raw)-sha256.Size:]) {
		return nil, fmt.Errorf("%w: %s.idx fails its checksum", ErrCorrupt, name)
	}
	if v := binary.BigEndian.Uint32(raw[len(idxMagic):]); v != packFormat {
		return nil, fmt.Errorf("cas: %s.idx: unknown pack format %d", name, v)
	}
	count := int(binary.BigEndian.Uint32(raw[len(idxMagic)+4:]))
	records := body[head : len(body)-sha256.Size]
	if len(records) != count*idxRecord {
		return nil, fmt.Errorf("%w: %s.idx holds %d bytes for %d objects", ErrCorrupt, name, len(records), count)
	}
	if want := "pack-" + hex.EncodeToString(body[len(body)-sha256.Size:]); want != name {
		return nil, fmt.Errorf("%w: %s.idx indexes %s", ErrCorrupt, name, want)
	}
	f, err := os.Open(filepath.Join(dir, name+".pack"))
	if err != nil {
		return nil, err
	}
	return &pack{name: name, path: filepath.Join(dir, name+".pack"), idx: records, f: f}, nil
}

// writePack writes objects into a new pack in dir, durably: the pack is
// fsynced and renamed, then its index, then the directory, so a visible
// index never names a pack the power loss took. Duplicate objects are
// written once. It returns the pack's name, or "" for no objects.
func writePack(dir string, objs []packObject) (string, error) {
	sort.Slice(objs, func(i, j int) bool {
		if objs[i].sp != objs[j].sp {
			return objs[i].sp < objs[j].sp
		}
		return objs[i].hash < objs[j].hash
	})
	uniq := objs[:0:0]
	for i, o := range objs {
		if i > 0 && o.sp == objs[i-1].sp && o.hash == objs[i-1].hash {
			continue
		}
		uniq = append(uniq, o)
	}
	if len(uniq) == 0 {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-pack-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(tmp, sum), 1<<20)
	var hdr [len(packMagic) + 8]byte
	copy(hdr[:], packMagic)
	binary.BigEndian.PutUint32(hdr[len(packMagic):], packFormat)
	binary.BigEndian.PutUint32(hdr[len(packMagic)+4:], uint32(len(uniq)))
	w.Write(hdr[:])
	off := int64(len(hdr))
	idx := make([]byte, 0, len(uniq)*idxRecord)
	for _, o := range uniq {
		d, err := digestOf(o.hash)
		if err != nil {
			tmp.Close()
			return "", err
		}
		var lb [binary.MaxVarintLen64]byte
		ln := binary.PutUvarint(lb[:], uint64(len(o.data)))
		w.WriteByte(byte(o.sp))
		w.Write(d)
		w.Write(lb[:ln])
		off += int64(1 + len(d) + ln)
		w.Write(o.data)
		rec := make([]byte, idxRecord)
		rec[0] = byte(o.sp)
		copy(rec[1:], d)
		binary.BigEndian.PutUint64(rec[1+sha256.Size:], uint64(off))
		binary.BigEndian.PutUint64(rec[1+sha256.Size+8:], uint64(len(o.data)))
		idx = append(idx, rec...)
		off += int64(len(o.data))
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return "", err
	}
	checksum := sum.Sum(nil)
	if _, err := tmp.Write(checksum); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	name := "pack-" + hex.EncodeToString(checksum)
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name+".pack")); err != nil {
		return "", err
	}
	var ib bytes.Buffer
	ib.WriteString(idxMagic)
	binary.Write(&ib, binary.BigEndian, uint32(packFormat))
	binary.Write(&ib, binary.BigEndian, uint32(len(uniq)))
	ib.Write(idx)
	ib.Write(checksum)
	isum := sha256.Sum256(ib.Bytes())
	ib.Write(isum[:])
	if err := writeFile(filepath.Join(dir, name+".idx"), ib.Bytes(), true); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return name, nil
}

// verifyPack reads a whole pack and checks its checksum, and that each
// object the index lists lies inside it and hashes to its name.
func verifyPack(p *pack) []error {
	var errs []error
	f, err := os.Open(p.path)
	if err != nil {
		return []error{err}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return []error{err}
	}
	if info.Size() < int64(len(packMagic)+8+sha256.Size) {
		return []error{fmt.Errorf("%w: %s.pack is cut short", ErrCorrupt, p.name)}
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(f, 0, info.Size()-sha256.Size)); err != nil {
		return []error{err}
	}
	tail := make([]byte, sha256.Size)
	if _, err := f.ReadAt(tail, info.Size()-sha256.Size); err != nil {
		return []error{err}
	}
	if !bytes.Equal(sum.Sum(nil), tail) || "pack-"+hex.EncodeToString(tail) != p.name {
		errs = append(errs, fmt.Errorf("%w: %s.pack fails its checksum", ErrCorrupt, p.name))
	}
	p.each(func(sp space, hash string, off, length int64) error {
		if off+length > info.Size()-sha256.Size {
			errs = append(errs, fmt.Errorf("%w: %s %s lies outside %s.pack", ErrCorrupt, sp, hash, p.name))
			return nil
		}
		data, err := p.read(off, length)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if hashBytes(data) != hash {
			errs = append(errs, fmt.Errorf("%w: %s object %s in %s.pack", ErrCorrupt, sp, hash, p.name))
		}
		return nil
	})
	return errs
}
