package cas

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// packObject is one object to put in a pack: its bytes, or where to
// read them. A pack of the whole store is written from locations, so
// what it costs in memory follows the number of objects and not their
// size; the bytes are read, and checked against their names, as the
// pack is written.
type packObject struct {
	sp   space
	hash string
	data []byte
	load func() ([]byte, error)
}

// corruptObject is an object whose bytes, read to be packed, failed
// their name or could not be read.
type corruptObject struct {
	sp   space
	hash string
	err  error
}

func (c *corruptObject) Error() string {
	return fmt.Sprintf("cas: %s object %s does not read: %v", c.sp, c.hash, c.err)
}

// writePackSkipping writes objs as writePack does. An object whose
// bytes fail to read or to match its name is taken from its next
// source in alts, when it has one, and otherwise left out and returned:
// such an object is left where it is, for Verify to report.
func writePackSkipping(o *objects, dir string, objs []packObject, alts map[objKey][]packObject) (string, []packObject, error) {
	var skipped []packObject
	for {
		name, err := writePack(o, dir, objs)
		var bad *corruptObject
		if !errors.As(err, &bad) {
			return name, skipped, err
		}
		k := objKey{bad.sp, bad.hash}
		keep := objs[:0:0]
		for _, o := range objs {
			if o.sp != bad.sp || o.hash != bad.hash {
				keep = append(keep, o)
				continue
			}
			if next := alts[k]; len(next) > 0 {
				keep = append(keep, next[0])
				alts[k] = next[1:]
				continue
			}
			skipped = append(skipped, o)
		}
		objs = keep
	}
}

// objKey names an object across spaces.
type objKey struct {
	sp   space
	hash string
}

// pack is an open pack: its index mapped, its file open for reads.
type pack struct {
	name string // pack-<hex>, without extension
	path string // the .pack file
	idx  []byte // the index records, sorted, in the mapping

	mu sync.RWMutex // held shared by a read, exclusive by close
	f  *os.File
	// data is the bytes of the pack before its checksum: an index record
	// naming anything outside them is damage, found before it is read.
	data int64

	// A lookup or walk of the index holds a reference, and the mapping
	// goes when a closed pack's last reference does, so no lookup reads
	// an index that is unmapped under it.
	refMu   sync.Mutex
	refs    int
	closing bool
	unmap   func() error
}

// acquire takes a reference to the index, or reports the pack closed.
func (p *pack) acquire() bool {
	p.refMu.Lock()
	defer p.refMu.Unlock()
	if p.closing {
		return false
	}
	p.refs++
	return true
}

func (p *pack) releaseRef() {
	p.refMu.Lock()
	defer p.refMu.Unlock()
	p.refs--
	if p.refs == 0 && p.closing && p.unmap != nil {
		p.unmap()
		p.unmap = nil
	}
}

// errPackClosed is a read from a pack dropped from the list since it
// was located; the reader looks again.
var errPackClosed = errors.New("cas: pack closed")

func digestOf(hash string) ([]byte, error) {
	d, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	if err != nil || len(d) != sha256.Size || !strings.HasPrefix(hash, "sha256:") {
		return nil, fmt.Errorf("%w: hash %q", ErrBadName, hash)
	}
	return d, nil
}

// find returns the offset and length of an object in the pack.
func (p *pack) find(sp space, digest []byte) (int64, int64, bool) {
	if !p.acquire() {
		return 0, 0, false
	}
	defer p.releaseRef()
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
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.f == nil {
		return nil, errPackClosed
	}
	if off < int64(len(packMagic)+8) || length < 0 || length > p.data-off {
		return nil, fmt.Errorf("%w: %s.pack: an index record names bytes %d+%d outside it", ErrCorrupt, p.name, off, length)
	}
	buf := make([]byte, length)
	if _, err := p.f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}

// each calls fn for every object the index lists.
func (p *pack) each(fn func(sp space, hash string, off, length int64) error) error {
	if !p.acquire() {
		return errPackClosed
	}
	defer p.releaseRef()
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
	p.mu.Lock()
	var err error
	if p.f != nil {
		err = p.f.Close()
		p.f = nil
	}
	p.mu.Unlock()
	p.refMu.Lock()
	defer p.refMu.Unlock()
	p.closing = true
	if p.refs == 0 && p.unmap != nil {
		p.unmap()
		p.unmap = nil
	}
	return err
}

// openPack maps a pack's index and opens the pack. It checks what can
// be checked without reading the index through: its header, that its
// length fits its count, and that it names this pack. The index's own
// checksum is Verify's to check, as git leaves it to verify-pack: a
// damaged record sends a lookup to bytes that fail their name, which a
// read reports, or misses an object, which Verify reports as missing.
func openPack(dir, name string) (*pack, error) {
	fi, err := os.Open(filepath.Join(dir, name+".idx"))
	if err != nil {
		return nil, err
	}
	defer fi.Close()
	info, err := fi.Stat()
	if err != nil {
		return nil, err
	}
	head := len(idxMagic) + 8
	if info.Size() < int64(head+2*sha256.Size) {
		return nil, fmt.Errorf("%w: %s.idx is not an index", ErrCorrupt, name)
	}
	raw, unmap, err := mapFile(fi, int(info.Size()))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*pack, error) {
		unmap()
		return nil, err
	}
	if string(raw[:len(idxMagic)]) != idxMagic {
		return fail(fmt.Errorf("%w: %s.idx is not an index", ErrCorrupt, name))
	}
	if v := binary.BigEndian.Uint32(raw[len(idxMagic):]); v != packFormat {
		return fail(fmt.Errorf("cas: %s.idx: unknown pack format %d", name, v))
	}
	count := int(binary.BigEndian.Uint32(raw[len(idxMagic)+4:]))
	body := raw[:len(raw)-sha256.Size]
	records := body[head : len(body)-sha256.Size]
	if len(records) != count*idxRecord {
		return fail(fmt.Errorf("%w: %s.idx holds %d bytes for %d objects", ErrCorrupt, name, len(records), count))
	}
	if want := "pack-" + hex.EncodeToString(body[len(body)-sha256.Size:]); want != name {
		return fail(fmt.Errorf("%w: %s.idx indexes %s", ErrCorrupt, name, want))
	}
	f, err := os.Open(filepath.Join(dir, name+".pack"))
	if err != nil {
		return fail(err)
	}
	pinfo, err := f.Stat()
	if err != nil || pinfo.Size() < int64(len(packMagic)+8+sha256.Size) {
		f.Close()
		return fail(fmt.Errorf("%w: %s.pack is cut short", ErrCorrupt, name))
	}
	return &pack{name: name, path: filepath.Join(dir, name+".pack"), idx: records, f: f, data: pinfo.Size() - sha256.Size, unmap: unmap}, nil
}

// verifyIndex checks a pack index's own checksum.
func verifyIndex(dir, name string) error {
	raw, err := os.ReadFile(filepath.Join(dir, name+".idx"))
	if err != nil {
		return err
	}
	if len(raw) < sha256.Size {
		return fmt.Errorf("%w: %s.idx is cut short", ErrCorrupt, name)
	}
	body := raw[:len(raw)-sha256.Size]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], raw[len(body):]) {
		return fmt.Errorf("%w: %s.idx fails its checksum", ErrCorrupt, name)
	}
	return nil
}

// writePack writes objects into a new pack in dir, durably: the pack is
// fsynced and renamed, then its index, then the directory, so a visible
// index never names a pack the power loss took. Duplicate objects are
// written once. An object given by location is read as it is written
// and checked against its name; one that fails stops the write with a
// *corruptObject. It returns the pack's name, or "" for no objects.
// Its fsyncs are o's, and o is nil for a pack of no store's.
func writePack(o *objects, dir string, objs []packObject) (string, error) {
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
		data := o.data
		if data == nil && o.load != nil {
			if data, err = o.load(); err == nil && hashBytes(data) != o.hash {
				err = ErrCorrupt
			}
			if err != nil {
				tmp.Close()
				return "", &corruptObject{o.sp, o.hash, err}
			}
		}
		var lb [binary.MaxVarintLen64]byte
		ln := binary.PutUvarint(lb[:], uint64(len(data)))
		w.WriteByte(byte(o.sp))
		w.Write(d)
		w.Write(lb[:ln])
		off += int64(1 + len(d) + ln)
		w.Write(data)
		var rec [idxRecord]byte
		rec[0] = byte(o.sp)
		copy(rec[1:], d)
		binary.BigEndian.PutUint64(rec[1+sha256.Size:], uint64(off))
		binary.BigEndian.PutUint64(rec[1+sha256.Size+8:], uint64(len(data)))
		idx = append(idx, rec[:]...)
		off += int64(len(data))
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
	if err := o.fsync(tmp.Name(), tmp, func(f *os.File) error { return f.Sync() }); err != nil {
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
	if _, err := o.writeFile(filepath.Join(dir, name+".idx"), ib.Bytes(), true); err != nil {
		return "", err
	}
	if err := o.fsyncDir(dir); err != nil {
		return "", err
	}
	return name, nil
}

// verifyPack reads a whole pack and checks its checksum, and that each
// object the index lists lies inside it and hashes to its name.
func verifyPack(p *pack) []error {
	var errs []error
	if err := verifyIndex(filepath.Dir(p.path), p.name); err != nil {
		errs = append(errs, err)
	}
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
