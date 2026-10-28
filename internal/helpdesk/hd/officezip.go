package hd

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"path/filepath"
	"strings"
)

// The ZIP-based office document families a file may name itself. A ZIP magic
// prefix alone says nothing about what a package holds (it can be a single
// program), so each extension below is accepted only when the archive has that
// family's content types, relationships and main document part.
type officeFamily struct {
	ext   string
	ooxml bool
	// mimetype is the exact ODF package media type ("mimetype" entry and manifest root).
	mimetype string
	// contentType is the OOXML main part content type, declared in [Content_Types].xml.
	contentType string
	// mainPart is the OOXML main document part the package relationship targets.
	mainPart string
	// nsToken is the namespace URI the main part's root element carries.
	nsToken string
}

var officeFamilies = map[string]officeFamily{
	".docx": {ext: ".docx", ooxml: true,
		contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml",
		mainPart:    "word/document.xml", nsToken: "wordprocessingml/2006/main"},
	".xlsx": {ext: ".xlsx", ooxml: true,
		contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml",
		mainPart:    "xl/workbook.xml", nsToken: "spreadsheetml/2006/main"},
	".pptx": {ext: ".pptx", ooxml: true,
		contentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml",
		mainPart:    "ppt/presentation.xml", nsToken: "presentationml/2006/main"},
	".odt": {ext: ".odt", mimetype: "application/vnd.oasis.opendocument.text"},
	".ods": {ext: ".ods", mimetype: "application/vnd.oasis.opendocument.spreadsheet"},
	".odp": {ext: ".odp", mimetype: "application/vnd.oasis.opendocument.presentation"},
}

// Package verification caps. Files are already held to MaxAttachmentBytes; the
// entry and decompression caps stop crafted archives (zip bombs, header floods)
// from costing work proportional to anything but the file itself.
const (
	maxPackageEntries = 65535 // the classic ZIP directory counts entries on uint16
	maxPartBytes      = 4 << 20
	maxInflateBytes   = 64 << 20
)

var (
	zipSigLocal      = [4]byte{'P', 'K', 3, 4}
	zipSigCentral    = [4]byte{'P', 'K', 1, 2}
	zipSigEOCD       = [4]byte{'P', 'K', 5, 6}
	zipSigDescriptor = [4]byte{'P', 'K', 7, 8}
)

// officePackageFamily picks the family an office-named, ZIP-headed file claims
// to belong to.
func officePackageFamily(filename string, head []byte) (officeFamily, bool) {
	f, ok := officeFamilies[strings.ToLower(filepath.Ext(filename))]
	if !ok || !bytes.HasPrefix(head, []byte(zip)) {
		return officeFamily{}, false
	}
	return f, true
}

// wants reports whether an entry is one of the parts the family's structure is
// proved from.
func (f officeFamily) wants(name string) bool {
	switch name {
	case "content.xml", "META-INF/manifest.xml", "mimetype":
		return !f.ooxml
	case "[Content_Types].xml", "_rels/.rels":
		return f.ooxml
	}
	return f.ooxml && name == f.mainPart
}

// VerifyOfficePackage checks that a ZIP-based office document (docx, xlsx,
// pptx, odt, ods, odp) is a complete package of the family its extension names:
// the OOXML content-types manifest, package relationships and main document part,
// or the ODF mimetype entry, manifest and content document. A plain archive or a
// package of another family cannot borrow the extension. head is the file's
// first bytes, already read; rest carries on after it. Nothing other than the
// bounded parts is held in memory. Files outside the six extensions, or without
// a ZIP head, are left to the first-bytes check and return nil.
func VerifyOfficePackage(head []byte, rest io.Reader, filename string) error {
	f, ok := officePackageFamily(filename, head)
	if !ok {
		return nil
	}
	return verifyOfficePackage(f, io.MultiReader(bytes.NewReader(head), rest))
}

// verifyOfficePackage scans the archive forward once: local file headers and
// their data, then the central directory and its end record, matching what it
// saw. It never seeks, so it runs on an upload stream.
func verifyOfficePackage(f officeFamily, r io.Reader) error {
	s := &packageScanner{
		pk:    &peekReader{r: io.LimitReader(r, MaxAttachmentBytes+1)},
		fam:   f,
		parts: map[string][]byte{},
	}
	sig, err := s.read(4)
	if err != nil || !bytes.Equal(sig, zipSigLocal[:]) {
		return ErrOfficePackageInvalid
	}
	for {
		if len(s.entries) >= maxPackageEntries {
			return ErrOfficePackageInvalid
		}
		if err := s.readLocalEntry(); err != nil {
			return err
		}
		sig, err = s.read(4)
		if err != nil {
			return ErrOfficePackageInvalid
		}
		switch {
		case bytes.Equal(sig, zipSigCentral[:]):
			if err := s.readCentralDirectory(); err != nil {
				return err
			}
			return s.validateFamily()
		case bytes.Equal(sig, zipSigLocal[:]):
		default:
			return ErrOfficePackageInvalid
		}
	}
}

type localEntry struct {
	name   string
	method uint16
	flags  uint16
	csize  int64 // compressed bytes actually consumed
	usize  int64 // uncompressed size declared in, or measured for, the entry
	offset int64 // local header offset
	extra  int   // extra-field length in the local header
}

type packageScanner struct {
	pk       *peekReader
	fam      officeFamily
	entries  []localEntry
	parts    map[string][]byte
	inflated int64
}

func (s *packageScanner) readLocalEntry() error {
	start := s.pk.n - 4
	h, err := s.read(26)
	if err != nil {
		return ErrOfficePackageInvalid
	}
	flags := binary.LittleEndian.Uint16(h[2:4])
	method := binary.LittleEndian.Uint16(h[4:6])
	csize := int64(binary.LittleEndian.Uint32(h[14:18]))
	usize := int64(binary.LittleEndian.Uint32(h[18:22]))
	nameLen := int(binary.LittleEndian.Uint16(h[22:24]))
	extraLen := int(binary.LittleEndian.Uint16(h[24:26]))
	if flags&0x0001 != 0 { // encrypted
		return ErrOfficePackageInvalid
	}
	if method != 0 && method != 8 { // store and deflate are all office packages use
		return ErrOfficePackageInvalid
	}
	name, err := s.read(nameLen)
	if err != nil {
		return ErrOfficePackageInvalid
	}
	if _, err := s.read(extraLen); err != nil {
		return ErrOfficePackageInvalid
	}
	e := localEntry{name: string(name), method: method, flags: flags, usize: usize, offset: start, extra: extraLen}
	lookup := partName(e.name)
	want := s.fam.wants(lookup)

	if flags&0x0008 == 0 {
		// Sizes are in the local header; read exactly the data it names.
		fr := &fixedReader{pk: s.pk, n: csize}
		raw, inflated, err := s.consume(method, fr, want)
		if err != nil || fr.n != 0 {
			return ErrOfficePackageInvalid
		}
		if method == 8 && (inflated != usize) {
			return ErrOfficePackageInvalid
		}
		e.csize = csize
		e.usize = usize
		if want {
			s.parts[lookup] = raw
		}
	} else {
		// Streaming writer: the length follows the data in a data descriptor. The
		// deflate reader, given a ByteReader, stops exactly at its stream end.
		dr := &ddReader{pk: s.pk}
		if method == 0 {
			n, err := scanStoredDescriptor(dr)
			dr.retract()
			if err != nil {
				return ErrOfficePackageInvalid
			}
			e.csize = n
			e.usize = n
		} else {
			raw, inflated, err := s.consume(method, dr, want)
			dr.retract()
			if err != nil {
				return ErrOfficePackageInvalid
			}
			dcsize, dusize, err := s.readDescriptor()
			if err != nil || dcsize != dr.n || dusize != inflated {
				return ErrOfficePackageInvalid
			}
			e.csize = dr.n
			e.usize = inflated
			if want {
				s.parts[lookup] = raw
			}
		}
	}
	s.entries = append(s.entries, e)
	return nil
}

// consume reads one entry's data, inflating it when deflated. It keeps at most
// maxPartBytes of it (the parts it inspects are small XML) and drains the rest,
// counting the uncompressed size against the bomb cap.
func (s *packageScanner) consume(method uint16, r io.Reader, want bool) (raw []byte, inflated int64, err error) {
	var in io.Reader = r
	var fr io.Closer
	if method == 8 {
		f := flate.NewReader(r)
		fr = f
		in = f
	}
	var captured int64
	if want {
		full, readErr := io.ReadAll(io.LimitReader(in, maxPartBytes+1))
		if readErr != nil {
			return nil, 0, ErrOfficePackageInvalid
		}
		// The structure markers always sit at the start of these parts; keep the
		// prefix and drain the rest rather than hold it all, but count every byte.
		captured = int64(len(full))
		if captured > maxPartBytes {
			raw = full[:maxPartBytes]
		} else {
			raw = full
		}
	}
	budget := int64(MaxAttachmentBytes) + 1
	if method == 8 {
		budget = maxInflateBytes + 1 - s.inflated
		if budget <= 0 {
			return nil, 0, ErrOfficePackageInvalid
		}
	}
	n, copyErr := io.Copy(io.Discard, io.LimitReader(in, budget))
	if copyErr != nil || n >= budget {
		return nil, 0, ErrOfficePackageInvalid
	}
	if fr != nil {
		if closeErr := fr.Close(); closeErr != nil {
			return nil, 0, ErrOfficePackageInvalid
		}
		s.inflated += captured + n
	}
	return raw, captured + n, nil
}

// readDescriptor consumes a data descriptor (PK0708 optional) after a streamed
// entry and returns the compressed and uncompressed sizes it records.
func (s *packageScanner) readDescriptor() (csize, usize int64, err error) {
	head, rerr := s.read(4)
	if rerr != nil {
		return 0, 0, rerr
	}
	var b [12]byte
	if bytes.Equal(head, zipSigDescriptor[:]) {
		if _, err := io.ReadFull(s.pk, b[:]); err != nil {
			return 0, 0, err
		}
	} else {
		copy(b[:4], head)
		if _, err := io.ReadFull(s.pk, b[4:]); err != nil {
			return 0, 0, err
		}
	}
	return int64(binary.LittleEndian.Uint32(b[4:8])), int64(binary.LittleEndian.Uint32(b[8:12])), nil
}

// scanStoredDescriptor finds a data descriptor after a stored streamed entry by
// its signature and checks the compressed size it records against the bytes
// read, so a signature inside the data cannot be mistaken for it.
func scanStoredDescriptor(d *ddReader) (int64, error) {
	var n int64
	matched := 0
	for {
		c, err := d.ReadByte()
		if err != nil {
			return 0, err
		}
		n++
		if c == zipSigDescriptor[matched] {
			matched++
			if matched == 4 {
				var fields [12]byte
				if _, err := io.ReadFull(d, fields[:]); err != nil {
					return 0, err
				}
				dataLen := n - 4
				if int64(binary.LittleEndian.Uint32(fields[4:8])) != dataLen {
					return 0, ErrOfficePackageInvalid
				}
				return dataLen, nil
			}
			continue
		}
		matched = 0
		if c == zipSigDescriptor[0] {
			matched = 1
		}
	}
}

// readCentralDirectory checks every central entry against the local entries and
// then the end-of-central-directory record: counts, names, methods, sizes,
// offsets and the directory position. A truncated or altered directory fails.
func (s *packageScanner) readCentralDirectory() error {
	cdStart := s.pk.n - 4
	for i := range s.entries {
		// The central header is 46 bytes including its signature, already read.
		h, err := s.read(42)
		if err != nil {
			return ErrOfficePackageInvalid
		}
		// In the bytes after the signature: method 6, sizes 16/20, name and
		// comment lengths 24..30, disk start 30, local offset 38.
		method := binary.LittleEndian.Uint16(h[6:8])
		csize := int64(binary.LittleEndian.Uint32(h[16:20]))
		usize := int64(binary.LittleEndian.Uint32(h[20:24]))
		nameLen := int(binary.LittleEndian.Uint16(h[24:26]))
		extraLen := int(binary.LittleEndian.Uint16(h[26:28]))
		commentLen := int(binary.LittleEndian.Uint16(h[28:30]))
		disk := binary.LittleEndian.Uint16(h[30:32])
		offset := int64(binary.LittleEndian.Uint32(h[38:42]))
		name, err := s.read(nameLen)
		if err != nil {
			return ErrOfficePackageInvalid
		}
		if _, err := s.read(extraLen + commentLen); err != nil {
			return ErrOfficePackageInvalid
		}
		e := s.entries[i]
		if disk != 0 || string(name) != e.name || method != e.method || csize != e.csize ||
			usize != e.usize || offset != e.offset {
			return ErrOfficePackageInvalid
		}
		sig, err := s.read(4)
		if err != nil {
			return ErrOfficePackageInvalid
		}
		switch {
		case bytes.Equal(sig, zipSigCentral[:]):
			continue
		case bytes.Equal(sig, zipSigEOCD[:]):
			return s.readEOCD(cdStart, i+1)
		default:
			return ErrOfficePackageInvalid
		}
	}
	// Zero entries can never be an office package; the EOCD is read below when
	// the first central entry turns up, so an empty directory fails here.
	return ErrOfficePackageInvalid
}

func (s *packageScanner) readEOCD(cdStart int64, entries int) error {
	h, err := s.read(18)
	if err != nil {
		return ErrOfficePackageInvalid
	}
	disk := binary.LittleEndian.Uint16(h[0:2])
	cdDisk := binary.LittleEndian.Uint16(h[2:4])
	diskEntries := binary.LittleEndian.Uint16(h[4:6])
	totalEntries := binary.LittleEndian.Uint16(h[6:8])
	cdSize := int64(binary.LittleEndian.Uint32(h[8:12]))
	cdOffset := int64(binary.LittleEndian.Uint32(h[12:16]))
	commentLen := int(binary.LittleEndian.Uint16(h[16:18]))
	if disk != 0 || cdDisk != 0 || int(diskEntries) != entries || int(totalEntries) != entries ||
		diskEntries == 0xffff || cdOffset == 0xffffffff || cdOffset != cdStart {
		return ErrOfficePackageInvalid
	}
	if _, err := s.read(commentLen); err != nil {
		return ErrOfficePackageInvalid
	}
	if cdSize != s.pk.n-cdStart-int64(22+commentLen) {
		return ErrOfficePackageInvalid
	}
	// Nothing follows a package's end record.
	tail, err := s.read(1)
	if err == nil && len(tail) > 0 {
		return ErrOfficePackageInvalid
	}
	return nil
}

func (s *packageScanner) validateFamily() error {
	f := s.fam
	if f.ooxml {
		ct := s.parts["[Content_Types].xml"]
		rels := s.parts["_rels/.rels"]
		main := s.parts[f.mainPart]
		if len(ct) == 0 || len(rels) == 0 || len(main) == 0 {
			return ErrOfficePackageInvalid
		}
		if rootLocalName(ct) != "Types" || !ooxmlOverride(ct, f.mainPart, f.contentType) {
			return ErrOfficePackageInvalid
		}
		if !officeDocumentRels(rels, f.mainPart) {
			return ErrOfficePackageInvalid
		}
		if rootLocalName(main) != strings.TrimSuffix(filepath.Base(f.mainPart), ".xml") ||
			!bytes.Contains(main, []byte(f.nsToken)) {
			return ErrOfficePackageInvalid
		}
		return nil
	}
	first := s.entries[0]
	// ODF 1.2: mimetype is first, stored, with no data descriptor or extra field.
	if first.name != "mimetype" || first.method != 0 || first.flags&0x0008 != 0 || first.extra != 0 {
		return ErrOfficePackageInvalid
	}
	mimetype := s.parts["mimetype"]
	content := s.parts["content.xml"]
	manifest := s.parts["META-INF/manifest.xml"]
	if !bytes.Equal(mimetype, []byte(f.mimetype)) || len(content) == 0 || len(manifest) == 0 {
		return ErrOfficePackageInvalid
	}
	if rootLocalName(content) != "document-content" ||
		!bytes.Contains(content, []byte("urn:oasis:names:tc:opendocument:xmlns:office:1.0")) ||
		!bytes.Contains(content, []byte(`office:mimetype="`+f.mimetype+`"`)) {
		return ErrOfficePackageInvalid
	}
	if rootLocalName(manifest) != "manifest" || !manifestRootType(manifest, f.mimetype) {
		return ErrOfficePackageInvalid
	}
	return nil
}

// partName normalises the "./" some writers prefix; the ODF mimetype name is
// matched literally by the caller.
func partName(name string) string {
	return strings.TrimPrefix(name, "./")
}

// ooxmlOverride reports whether [Content_Types].xml declares an Override for
// mainPart with the family's main content type.
func ooxmlOverride(ct []byte, mainPart, contentType string) bool {
	marker := []byte("<Override")
	for {
		i := bytes.Index(ct, marker)
		if i < 0 {
			return false
		}
		el, rest, ok := cutElement(ct[i:])
		if !ok {
			return false
		}
		if bytes.Contains(el, []byte(contentType)) && attrValue(el, "PartName") == "/"+mainPart {
			return true
		}
		ct = rest
	}
}

// officeDocumentRels reports whether _rels/.rels relates the main document part
// with the officeDocument relationship type.
func officeDocumentRels(rels []byte, mainPart string) bool {
	const relType = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument"
	marker := []byte("<Relationship")
	for {
		i := bytes.Index(rels, marker)
		if i < 0 {
			return false
		}
		el, rest, ok := cutElement(rels[i:])
		if !ok {
			return false
		}
		if attrValue(el, "Type") == relType {
			target := strings.TrimPrefix(attrValue(el, "Target"), "/")
			if strings.EqualFold(target, mainPart) {
				return true
			}
		}
		rels = rest
	}
}

// manifestRootType reports whether META-INF/manifest.xml declares the package
// media type on the root "/" file entry.
func manifestRootType(manifest []byte, mimetype string) bool {
	marker := []byte("<manifest:file-entry")
	for {
		i := bytes.Index(manifest, marker)
		if i < 0 {
			return false
		}
		el, rest, ok := cutElement(manifest[i:])
		if !ok {
			return false
		}
		if attrValue(el, "manifest:full-path") == "/" && attrValue(el, "manifest:media-type") == mimetype {
			return true
		}
		manifest = rest
	}
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// cutElement returns the bytes of the element starting at b (up to and excluding
// the closing '>') and what follows it.
func cutElement(b []byte) (el, rest []byte, ok bool) {
	j := bytes.IndexByte(b, '>')
	if j < 0 {
		return nil, nil, false
	}
	return b[:j], b[j+1:], true
}

// attrValue reads an attribute value from an element's bytes, accepting either
// quote, without parsing XML: the parts are structure-checked, never rendered.
func attrValue(el []byte, name string) string {
	for i := 0; ; {
		j := bytes.Index(el[i:], []byte(name))
		if j < 0 {
			return ""
		}
		pos := i + j
		i = pos + len(name)
		// The match must be the whole attribute name, not a suffix of another
		// (ContentType/Type, TargetMode/Target).
		if pos > 0 && !isSpaceByte(el[pos-1]) {
			continue
		}
		for i < len(el) && isSpaceByte(el[i]) {
			i++
		}
		if i >= len(el) || el[i] != '=' {
			continue
		}
		i++
		for i < len(el) && isSpaceByte(el[i]) {
			i++
		}
		if i >= len(el) || el[i] != '"' && el[i] != '\'' {
			continue
		}
		q := el[i]
		i++
		end := bytes.IndexByte(el[i:], q)
		if end < 0 {
			return ""
		}
		return string(el[i : i+end])
	}
}

// rootLocalName is the local name of the first element in an XML document,
// tolerating a BOM, declaration, comments and whitespace and any prefix.
func rootLocalName(b []byte) string {
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("<?xml")) {
		if i := bytes.IndexByte(b, '>'); i >= 0 {
			b = b[i+1:]
		}
	}
	for {
		i := bytes.IndexByte(b, '<')
		if i < 0 {
			return ""
		}
		if bytes.HasPrefix(b[i:], []byte("<!--")) {
			if j := bytes.Index(b[i:], []byte("-->")); j >= 0 {
				b = b[i+j+3:]
				continue
			}
		}
		if bytes.HasPrefix(b[i:], []byte("<?")) || bytes.HasPrefix(b[i:], []byte("<!")) {
			if j := bytes.IndexByte(b[i:], '>'); j >= 0 {
				b = b[i+j+1:]
				continue
			}
		}
		b = b[i+1:]
		break
	}
	end := bytes.IndexAny(b, " \t\r\n/>")
	if end < 0 {
		end = len(b)
	}
	name := b[:end]
	if j := bytes.LastIndexByte(name, ':'); j >= 0 {
		name = name[j+1:]
	}
	return string(name)
}

func (s *packageScanner) read(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.pk, b); err != nil {
		return nil, err
	}
	return b, nil
}

// peekReader serves a stream with bytes prepended back (a chunk reader fetched
// past the end of a streamed entry) and counts the logical archive position:
// prepended bytes are un-consumed, then counted again as they are re-served.
type peekReader struct {
	r    io.Reader
	back []byte
	n    int64
}

func (p *peekReader) Read(dst []byte) (int, error) {
	if len(p.back) > 0 {
		m := copy(dst, p.back)
		p.back = p.back[m:]
		p.n += int64(m)
		return m, nil
	}
	m, err := p.r.Read(dst)
	p.n += int64(m)
	return m, err
}

func (p *peekReader) prepend(b []byte) {
	if len(b) > 0 {
		p.n -= int64(len(b))
		p.back = append(append(make([]byte, 0, len(b)+len(p.back)), b...), p.back...)
	}
}

// fixedReader reads at most n bytes of a known-length local entry, one byte at a
// time on demand so a deflate reader cannot reach into the next entry.
type fixedReader struct {
	pk *peekReader
	n  int64
}

func (f *fixedReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > f.n {
		p = p[:f.n]
	}
	m, err := f.pk.Read(p)
	f.n -= int64(m)
	if err == io.EOF && f.n > 0 {
		err = io.ErrUnexpectedEOF
	}
	return m, err
}

func (f *fixedReader) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// ddReader serves a streamed entry from buffered chunks (flate needs a
// ByteReader to stop at the exact stream end) and gives unconsumed bytes back
// to the archive on retract.
type ddReader struct {
	pk    *peekReader
	buf   []byte
	pos   int
	n     int64
	chunk [32 << 10]byte
}

func (d *ddReader) refill() error {
	if d.pos < len(d.buf) {
		return nil
	}
	m, err := d.pk.Read(d.chunk[:])
	d.buf = d.chunk[:m]
	d.pos = 0
	if m == 0 {
		return err
	}
	return nil
}

func (d *ddReader) Read(p []byte) (int, error) {
	if err := d.refill(); err != nil {
		return 0, err
	}
	m := copy(p, d.buf[d.pos:])
	d.pos += m
	d.n += int64(m)
	return m, nil
}

func (d *ddReader) ReadByte() (byte, error) {
	if err := d.refill(); err != nil {
		return 0, err
	}
	b := d.buf[d.pos]
	d.pos++
	d.n++
	return b, nil
}

func (d *ddReader) retract() {
	if d.pos < len(d.buf) {
		d.pk.prepend(d.buf[d.pos:])
		d.pos = len(d.buf)
	}
}
