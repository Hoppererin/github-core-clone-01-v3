package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ---------------------------------------------------------------------------
// pkt-line framing (git smart HTTP)
// ---------------------------------------------------------------------------

// readPktLine reads one pkt-line from b at offset off. Returns the payload
// (nil for flush), the new offset, and a kind: 0 = data, 1 = flush,
// 2 = delim, 3 = response-end.
func readPktLine(b []byte, off int) (payload []byte, next int, kind int, err error) {
	if off+4 > len(b) {
		return nil, off, 0, io.ErrUnexpectedEOF
	}
	var n int
	for i := 0; i < 4; i++ {
		c := b[off+i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return nil, off, 0, fmt.Errorf("invalid pkt-line length %q", string(b[off:off+4]))
		}
		n = n*16 + v
	}
	if n == 0 {
		return nil, off + 4, 1, nil
	}
	if n == 1 {
		return nil, off + 4, 2, nil
	}
	if n == 2 {
		return nil, off + 4, 3, nil
	}
	if n < 4 || off+n > len(b) {
		return nil, off, 0, io.ErrUnexpectedEOF
	}
	return b[off+4 : off+n], off + n, 0, nil
}

func pktLine(payload []byte) []byte {
	out := make([]byte, 4, len(payload)+4)
	n := len(payload) + 4
	hexDigits := "0123456789abcdef"
	out[0] = hexDigits[(n>>12)&0xf]
	out[1] = hexDigits[(n>>8)&0xf]
	out[2] = hexDigits[(n>>4)&0xf]
	out[3] = hexDigits[n&0xf]
	return append(out, payload...)
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// ---------------------------------------------------------------------------
// Pack reading
// ---------------------------------------------------------------------------

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
	objOfsD   = 6
	objRefD   = 7
)

var objTypeName = map[int]string{
	objCommit: "commit",
	objTree:   "tree",
	objBlob:   "blob",
	objTag:    "tag",
}

var objTypeCode = map[string]int{
	"commit": objCommit,
	"tree":   objTree,
	"blob":   objBlob,
	"tag":    objTag,
}

// packEntry is one parsed pack entry before delta resolution.
type packEntry struct {
	offset int // byte offset of the entry in the pack
	typ    int
	size   int64
	data   []byte // inflated content (for deltas: the delta instructions)
	base   string // for ref-delta: base oid hex; for ofs-delta: "" (use baseOff)
	baseOff int   // for ofs-delta: negative offset from this entry
	oid    string // resolved once the object is known
}

// readVarint reads a git pack size/type varint. Returns type, size, bytes used.
func readPackVarint(b []byte, off int) (typ int, size int64, used int, err error) {
	if off >= len(b) {
		return 0, 0, 0, io.ErrUnexpectedEOF
	}
	c := b[off]
	typ = int((c >> 4) & 0x7)
	size = int64(c & 0x0f)
	shift := uint(4)
	used = 1
	for c&0x80 != 0 {
		if off+used >= len(b) {
			return 0, 0, 0, io.ErrUnexpectedEOF
		}
		c = b[off+used]
		used++
		size |= int64(c&0x7f) << shift
		shift += 7
	}
	return typ, size, used, nil
}

// inflateAt inflates one zlib stream starting at b[off]. It returns the
// inflated bytes and the offset just past the zlib stream. The bytes.Reader
// is an io.ByteReader, so flate consumes exactly the stream's bytes.
func inflateAt(b []byte, off int) ([]byte, int, error) {
	r := bytes.NewReader(b[off:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, off, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, off, err
	}
	consumed := len(b[off:]) - r.Len()
	return data, off + consumed, nil
}

// parsePack parses a whole packfile. It returns the entries in pack order
// with deltas resolved to (type, data). Objects whose delta base is not in
// the pack are resolved against the store (s may be nil to disallow that).
func parsePack(pack []byte, s *Store) ([]*packEntry, error) {
	if len(pack) < 12 || string(pack[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	off := 12
	entries := make([]*packEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		if off >= len(pack) {
			return nil, io.ErrUnexpectedEOF
		}
		start := off
		typ, size, used, err := readPackVarint(pack, off)
		if err != nil {
			return nil, err
		}
		off += used
		e := &packEntry{offset: start, typ: typ, size: size}
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			data, next, err := inflateAt(pack, off)
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != size {
				return nil, fmt.Errorf("object size mismatch: header %d, inflated %d", size, len(data))
			}
			e.data = data
			e.oid = hashObject(objTypeName[typ], data)
			off = next
		case objOfsD:
			// offset encoding: 7 bits per byte, big-endian with +1 per byte
			if off >= len(pack) {
				return nil, io.ErrUnexpectedEOF
			}
			c := pack[off]
			off++
			baseOff := int(c & 0x7f)
			for c&0x80 != 0 {
				if off >= len(pack) {
					return nil, io.ErrUnexpectedEOF
				}
				c = pack[off]
				off++
				baseOff = ((baseOff + 1) << 7) | int(c&0x7f)
			}
			e.baseOff = -baseOff
			data, next, err := inflateAt(pack, off)
			if err != nil {
				return nil, err
			}
			e.data = data
			off = next
		case objRefD:
			if off+20 > len(pack) {
				return nil, io.ErrUnexpectedEOF
			}
			e.base = hex.EncodeToString(pack[off : off+20])
			off += 20
			data, next, err := inflateAt(pack, off)
			if err != nil {
				return nil, err
			}
			e.data = data
			off = next
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
		entries = append(entries, e)
	}
	// Resolve deltas. Bases precede their deltas in a well-formed pack, but
	// resolve iteratively to be safe (delta chains).
	byOffset := map[int]*packEntry{}
	for _, e := range entries {
		byOffset[e.offset] = e
	}
	byOID := map[string]*packEntry{}
	for _, e := range entries {
		if e.oid != "" {
			byOID[e.oid] = e
		}
	}
	resolved := map[*packEntry]bool{}
	for _, e := range entries {
		if e.typ != objOfsD && e.typ != objRefD {
			resolved[e] = true
		}
	}
	for round := 0; round < len(entries)+2; round++ {
		progress := false
		for _, e := range entries {
			if resolved[e] {
				continue
			}
			var base *packEntry
			var baseObj *Object
			if e.typ == objOfsD {
				base = byOffset[e.offset+e.baseOff]
			} else {
				base = byOID[e.base]
				if base == nil && s != nil {
					s.mu.RLock()
					baseObj = s.objects[e.base]
					s.mu.RUnlock()
				}
			}
			if base != nil && !resolved[base] {
				continue
			}
			var baseType string
			var baseData []byte
			if base != nil {
				baseType = objTypeName[base.typ]
				baseData = base.data
			} else if baseObj != nil {
				baseType = baseObj.Type
				baseData = baseObj.Data
			} else {
				return nil, fmt.Errorf("delta base not found for entry at %d", e.offset)
			}
			out, err := applyDelta(baseData, e.data)
			if err != nil {
				return nil, err
			}
			e.typ = objTypeCode[baseType]
			e.data = out
			e.oid = hashObject(baseType, out)
			byOID[e.oid] = e
			resolved[e] = true
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, e := range entries {
		if !resolved[e] {
			return nil, fmt.Errorf("unresolved delta at offset %d", e.offset)
		}
	}
	return entries, nil
}

// applyDelta applies git delta instructions to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, io.ErrUnexpectedEOF
			}
			c := delta[off]
			off++
			v |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size mismatch: %d vs %d", srcSize, len(base))
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out []byte
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			// copy from base
			var cpOff, cpSize int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, io.ErrUnexpectedEOF
					}
					cpOff |= int64(delta[off]) << uint(8*i)
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, io.ErrUnexpectedEOF
					}
					cpSize |= int64(delta[off]) << uint(8*i)
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > int64(len(base)) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			// insert literal
			n := int(op)
			if off+n > len(delta) {
				return nil, io.ErrUnexpectedEOF
			}
			out = append(out, delta[off:off+n]...)
			off += n
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if int64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch: %d vs %d", len(out), tgtSize)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pack writing (plain objects, no deltas)
// ---------------------------------------------------------------------------

// writePack builds a packfile holding the given object ids (in the given
// order) from the store. Caller must hold at least a read lock.
func writePack(s *Store, oids []string) []byte {
	// deterministic order: sort for stable output
	sorted := make([]string, len(oids))
	copy(sorted, oids)
	sort.Strings(sorted)
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(sorted)))
	buf.Write(hdr[:])
	for _, oid := range sorted {
		o := s.objects[oid]
		if o == nil {
			continue
		}
		writePackEntry(&buf, objTypeCode[o.Type], o.Data)
	}
	// trailer: sha-1 of everything so far
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writePackEntry(buf *bytes.Buffer, typ int, data []byte) {
	size := len(data)
	// first byte: type in bits 4-6, low 4 bits of size
	b0 := byte((typ << 4) | (size & 0x0f))
	size >>= 4
	if size > 0 {
		b0 |= 0x80
	}
	buf.WriteByte(b0)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
	zw := zlib.NewWriter(buf)
	zw.Write(data)
	zw.Close()
}

// sideband wraps pack bytes into side-band-64k packets (band 1). The whole
// pack is chunked so each pkt-line stays within the 65520-byte limit.
func sidebandPack(pack []byte) []byte {
	var out []byte
	const maxData = 65515 // 65520 - 4 (length) - 1 (band byte)
	for len(pack) > 0 {
		n := len(pack)
		if n > maxData {
			n = maxData
		}
		chunk := make([]byte, 0, n+5)
		chunk = pktLine(append([]byte{1}, pack[:n]...))
		out = append(out, chunk...)
		pack = pack[n:]
	}
	return out
}
