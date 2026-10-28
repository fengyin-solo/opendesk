package hd_test

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/useopendesk/opendesk/internal/helpdesk/hd"
	"github.com/useopendesk/opendesk/internal/helpdesk/hdtest"
	"github.com/useopendesk/opendesk/internal/storage"
)

// officeExts are the six ZIP-based office extensions.
var officeExts = []string{".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp"}

// testFile is one entry of a package a test builds. raw writes a classic local
// header with known sizes (CreateRaw); otherwise the entry is streamed with a
// data descriptor, the way Java and Go's archive/zip write them.
type testFile struct {
	name  string
	data  []byte
	store bool
	raw   bool
}

func deflateBytes(t *testing.T, src []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipBuild(t *testing.T, files []testFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		if f.raw {
			fh := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
			payload := deflateBytes(t, f.data)
			if f.store {
				fh.Method = zip.Store
				payload = f.data
			}
			fh.CRC32 = crc32.ChecksumIEEE(f.data)
			fh.CompressedSize64 = uint64(len(payload))
			fh.UncompressedSize64 = uint64(len(f.data))
			w, err := zw.CreateRaw(fh)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatal(err)
			}
			continue
		}
		method := zip.Deflate
		if f.store {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var ooxmlMinimal = map[string]struct {
	part, contentType, prefix, root, ns string
}{
	".docx": {"word/document.xml",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml",
		"w", "document", "wordprocessingml/2006/main"},
	".xlsx": {"xl/workbook.xml",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml",
		"x", "workbook", "spreadsheetml/2006/main"},
	".pptx": {"ppt/presentation.xml",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml",
		"p", "presentation", "presentationml/2006/main"},
}

func ooxmlFiles(ext string) []testFile {
	m := ooxmlMinimal[ext]
	contentTypes := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/%s" ContentType="%s"/>
</Types>`, m.part, m.contentType)
	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="` + m.part + `"/>
</Relationships>`
	main := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<%s:%s xmlns:%s="http://schemas.openxmlformats.org/officeDocument/2006/relationships/../%s"><%s:body/></%s:%s>`,
		m.prefix, m.root, m.prefix, m.ns, m.prefix, m.prefix, m.root)
	return []testFile{
		{name: "[Content_Types].xml", data: []byte(contentTypes)},
		{name: "_rels/.rels", data: []byte(rels)},
		{name: m.part, data: []byte(main)},
	}
}

var odfMimetype = map[string]string{
	".odt": "application/vnd.oasis.opendocument.text",
	".ods": "application/vnd.oasis.opendocument.spreadsheet",
	".odp": "application/vnd.oasis.opendocument.presentation",
}

func odfFiles(ext string) []testFile {
	mt := odfMimetype[ext]
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" office:version="1.2" office:mimetype="%s"><office:body/></office:document-content>`, mt)
	manifest := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2">
<manifest:file-entry manifest:full-path="/" manifest:version="1.2" manifest:media-type="%s"/>
<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>
</manifest:manifest>`, mt)
	// The mimetype entry is first, stored, with no data descriptor or extra
	// field, as ODF requires; the other parts stream with a data descriptor.
	return []testFile{
		{name: "mimetype", data: []byte(mt), store: true, raw: true},
		{name: "content.xml", data: []byte(content)},
		{name: "META-INF/manifest.xml", data: []byte(manifest)},
	}
}

func minimalPackage(t *testing.T, ext string) []byte {
	t.Helper()
	if _, ok := ooxmlMinimal[ext]; ok {
		return zipBuild(t, ooxmlFiles(ext))
	}
	return zipBuild(t, odfFiles(ext))
}

func verifyPkg(data []byte, name string) error {
	h := min(len(data), 4096)
	return hd.VerifyOfficePackage(data[:h], bytes.NewReader(data[h:]), name)
}

func TestVerifyMinimalOfficePackages(t *testing.T) {
	for _, ext := range officeExts {
		pkg := minimalPackage(t, ext)
		if err := verifyPkg(pkg, "file"+ext); err != nil {
			t.Errorf("%s valid minimal package refused: %v", ext, err)
		}
		// A package cannot borrow another family's extension.
		for _, other := range officeExts {
			if other == ext {
				continue
			}
			if err := verifyPkg(pkg, "file"+other); err == nil {
				t.Errorf("%s package accepted as %s", ext, other)
			}
		}
	}

	// The same packages with classic local headers (known sizes, no data
	// descriptors) are accepted too: Office and LibreOffice write these.
	for _, ext := range []string{".docx", ".xlsx", ".pptx"} {
		files := ooxmlFiles(ext)
		for i := range files {
			files[i].raw = true
		}
		if err := verifyPkg(zipBuild(t, files), "file"+ext); err != nil {
			t.Errorf("classic %s refused: %v", ext, err)
		}
	}
	for _, ext := range []string{".odt", ".ods", ".odp"} {
		files := odfFiles(ext)
		for i := range files[1:] {
			files[i+1].raw = true
		}
		if err := verifyPkg(zipBuild(t, files), "file"+ext); err != nil {
			t.Errorf("classic %s refused: %v", ext, err)
		}
	}
}

func TestPlainZipCannotBorrowOfficeName(t *testing.T) {
	plain := zipBuild(t, []testFile{
		{name: "evil.exe", data: []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff")},
		{name: "run.sh", data: []byte("#!/bin/sh\nrm -rf /")},
	})
	for _, ext := range officeExts {
		if err := verifyPkg(plain, "invoice"+ext); err == nil {
			t.Errorf("plain ZIP accepted as %s", ext)
		}
	}
	// A ZIP is still a ZIP under its own name.
	if err := verifyPkg(plain, "archive.zip"); err != nil {
		t.Errorf("plain .zip refused: %v", err)
	}
	if err := verifyPkg(plain, "photo.png"); err != nil {
		t.Errorf("non-office name checked: %v", err)
	}

	// The executable and script refusals still stand on their own.
	if !hd.IsExecutable(plain, "invoice.exe") {
		t.Error("ZIP with an .exe name not refused")
	}
	if !hd.IsExecutable([]byte("#!/bin/sh\n"), "invoice.docx") {
		t.Error("script with a .docx name not refused")
	}
}

func TestCorruptCentralDirectory(t *testing.T) {
	pkg := minimalPackage(t, ".docx")
	cd := bytes.Index(pkg[4:], []byte("PK\x01\x02")) + 4

	for name, raw := range map[string][]byte{
		"central directory missing": pkg[:cd],
		"end record missing":        pkg[:len(pkg)-22],
		"trailing bytes":            append(append([]byte{}, pkg...), 'j', 'u', 'n', 'k'),
	} {
		if err := verifyPkg(raw, "file.docx"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	// Alter a name inside the central directory: it no longer matches its entry.
	flipped := append([]byte{}, pkg...)
	nameLen := int(flipped[cd+28]) | int(flipped[cd+29])<<8
	if nameLen == 0 {
		t.Fatal("central entry has no name")
	}
	flipped[cd+46] ^= 0x01
	if err := verifyPkg(flipped, "file.docx"); err == nil {
		t.Error("central directory with a changed name accepted")
	}

	// An unknown compression method and an encrypted entry are refused.
	badMethod := append([]byte{}, pkg...)
	badMethod[8] = 0x09 // deflate64, which office packages never use
	if err := verifyPkg(badMethod, "file.docx"); err == nil {
		t.Error("unsupported compression accepted")
	}
	encrypted := append([]byte{}, pkg...)
	encrypted[6] |= 0x01
	if err := verifyPkg(encrypted, "file.docx"); err == nil {
		t.Error("encrypted entry accepted")
	}
}

func TestEmptyAndNonOfficeFiles(t *testing.T) {
	// An empty file is no kind of package and fails the head gate and upload.
	if hd.FileTypeAllowed(nil, "file.docx", officeExts) {
		t.Error("empty file allowed")
	}
	if _, err := hd.SaveUpload(context.Background(), nil, nil, hd.Upload{Filename: "file.docx", Size: 0}); err == nil {
		t.Error("empty upload accepted")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty upload: %v", err)
	}

	// Existing first-bytes behaviour is untouched: package verification never
	// changes the verdict for images, PDF, text or audio.
	keep := []struct {
		name, head string
	}{
		{"shot.png", "\x89PNG\r\n\x1a\n\x00\x00"},
		{"photo.jpg", "\xff\xd8\xff\xe0"},
		{"invoice.pdf", "%PDF-1.7\n"},
		{"notes.txt", "Hello, this is plain text.\n"},
		{"data.csv", "id,name\n1,Ada\n"},
		{"voice.mp3", "ID3\x04\x00"},
		{"call.wav", "RIFF\x24\x00\x00\x00WAVEfmt "},
		{"voice.webm", "\x1a\x45\xdf\xa3\x9f\x42\x86\x81"},
	}
	all := []string{"png", "jpg", "pdf", "txt", "csv", "mp3", "wav", "webm",
		"docx", "xlsx", "pptx", "odt", "ods", "odp", "zip"}
	for _, c := range keep {
		if !hd.FileTypeAllowed([]byte(c.head), c.name, all) {
			t.Errorf("%s refused by the head gate", c.name)
		}
		if err := verifyPkg([]byte(c.head), c.name); err != nil {
			t.Errorf("%s package-checked: %v", c.name, err)
		}
	}
	refused := []struct {
		name, head string
	}{
		{"shell.png", "#!/bin/sh\nrm -rf /"},
		{"tool.pdf", "MZ\x90\x00"},
		{"run.jpg", "\x7fELF\x02\x01"},
	}
	for _, c := range refused {
		if hd.FileTypeAllowed([]byte(c.head), c.name, all) {
			t.Errorf("%s allowed", c.name)
		}
	}
}

func TestOOXMLStructureRequired(t *testing.T) {
	files := ooxmlFiles(".docx")
	good := zipBuild(t, files)

	// Remove the main part override from content types.
	withoutOverride := zipBuild(t, withEntry(files, "[Content_Types].xml", func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte(ooxmlMinimal[".docx"].contentType), []byte("application/wrong"))
	}))
	if err := verifyPkg(withoutOverride, "file.docx"); err == nil {
		t.Error("wrong content type accepted")
	}

	// Relationship targets a different part.
	wrongRels := zipBuild(t, withEntry(files, "_rels/.rels", func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte(`Target="word/document.xml"`), []byte(`Target="xl/workbook.xml"`))
	}))
	if err := verifyPkg(wrongRels, "file.docx"); err == nil {
		t.Error("relationship to another family's part accepted")
	}

	// Main part carries a different family's namespace.
	wrongNS := zipBuild(t, withEntry(files, "word/document.xml", func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte("wordprocessingml/2006/main"), []byte("spreadsheetml/2006/main"))
	}))
	if err := verifyPkg(wrongNS, "file.docx"); err == nil {
		t.Error("wrong main document namespace accepted")
	}

	for _, drop := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"} {
		var partial []testFile
		for _, f := range files {
			if f.name != drop {
				partial = append(partial, f)
			}
		}
		if err := verifyPkg(zipBuild(t, partial), "file.docx"); err == nil {
			t.Errorf("package without %s accepted", drop)
		}
	}

	// Garbage deflate data for the main part is not repaired.
	corrupt := corruptEntryData(t, good, "word/document.xml")
	if err := verifyPkg(corrupt, "file.docx"); err == nil {
		t.Error("corrupt deflate stream accepted")
	}
}

func TestODFStructureRequired(t *testing.T) {
	files := odfFiles(".odt")

	// mimetype streamed (data descriptor and extra field) instead of first/stored.
	streamedMime := cloneFiles(files)
	streamedMime[0].raw = false
	if err := verifyPkg(zipBuild(t, streamedMime), "file.odt"); err == nil {
		t.Error("streamed mimetype accepted")
	}

	// mimetype not first.
	reordered := cloneFiles(files)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if err := verifyPkg(zipBuild(t, reordered), "file.odt"); err == nil {
		t.Error("mimetype not first accepted")
	}

	// mimetype names another family.
	wrongMime := withEntry(files, "mimetype", func([]byte) []byte {
		return []byte(odfMimetype[".ods"])
	})
	wrongMime[0].raw = true
	wrongMime[0].store = true
	if err := verifyPkg(zipBuild(t, wrongMime), "file.odt"); err == nil {
		t.Error("spreadsheet mimetype accepted as text")
	}

	// Manifest without the root media type.
	noRoot := withEntry(files, "META-INF/manifest.xml", func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte(`manifest:full-path="/"`), []byte(`manifest:full-path="/wrong"`))
	})
	if err := verifyPkg(zipBuild(t, noRoot), "file.odt"); err == nil {
		t.Error("manifest without root media type accepted")
	}

	// content.xml carries another mimetype.
	badContent := withEntry(files, "content.xml", func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte(odfMimetype[".odt"]), []byte(odfMimetype[".odp"]))
	})
	if err := verifyPkg(zipBuild(t, badContent), "file.odt"); err == nil {
		t.Error("content.xml of another family accepted")
	}

	// Missing parts.
	for _, drop := range []string{"mimetype", "content.xml", "META-INF/manifest.xml"} {
		var partial []testFile
		for _, f := range files {
			if f.name != drop {
				partial = append(partial, f)
			}
		}
		if err := verifyPkg(zipBuild(t, partial), "file.odt"); err == nil {
			t.Errorf("package without %s accepted", drop)
		}
	}
}

func TestZipBombRejected(t *testing.T) {
	files := ooxmlFiles(".docx")
	big := bytes.Repeat([]byte{'A'}, 70<<20) // deflates to a few KB, expands past the cap
	files = append(files, testFile{name: "word/embeddings/blob.bin", data: big})
	pkg := zipBuild(t, files)
	if len(pkg) > hd.MaxAttachmentBytes {
		t.Fatalf("bomb fixture too large: %d", len(pkg))
	}
	if err := verifyPkg(pkg, "file.docx"); err == nil {
		t.Error("decompression bomb accepted")
	}
}

func TestSaveUploadOfficePackages(t *testing.T) {
	e := hdtest.New(t)
	ws := e.NewWorkspace()
	wsID := ws.ID
	dir := t.TempDir()
	st, err := storage.New(context.Background(), storage.Options{Driver: "local", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	// A valid package is stored with its document content type.
	doc := minimalPackage(t, ".docx")
	att, err := hd.SaveUpload(e.Ctx, e.D.Storage, e.D.DB.Pool, hd.Upload{
		WorkspaceID: wsID, Filename: "report.docx", Size: int64(len(doc)), Body: bytes.NewReader(doc)})
	if err != nil {
		t.Fatalf("valid docx: %v", err)
	}
	if !strings.Contains(att.ContentType, "wordprocessingml") {
		t.Errorf("content type: %s", att.ContentType)
	}

	// A plain ZIP under a document name is refused and its object removed.
	plain := zipBuild(t, []testFile{{name: "evil.exe", data: []byte("MZ\x90\x00")}})
	_, err = hd.SaveUpload(e.Ctx, st, nil, hd.Upload{
		WorkspaceID: wsID, Filename: "invoice.docx", Size: int64(len(plain)), Body: bytes.NewReader(plain)})
	if !errors.Is(err, hd.ErrOfficePackageInvalid) {
		t.Fatalf("fake docx: %v", err)
	}

	// The same verdict applies with a type-restricted accept: the head gate
	// passes, the package gate is what refuses it.
	if !hd.FileTypeAllowed(doc[:8], "report.docx", []string{"docx"}) {
		t.Fatal("head gate")
	}
	accept := hd.AcceptTypes([]string{"docx"})
	_, err = hd.SaveUpload(e.Ctx, st, nil, hd.Upload{
		WorkspaceID: wsID, Filename: "invoice.docx", Size: int64(len(plain)),
		Body: bytes.NewReader(plain), Accept: accept})
	if !errors.Is(err, hd.ErrOfficePackageInvalid) {
		t.Fatalf("fake docx under accept: %v", err)
	}

	// ODF is verified the same way.
	odt := minimalPackage(t, ".odt")
	if _, err := hd.SaveUpload(e.Ctx, e.D.Storage, e.D.DB.Pool, hd.Upload{
		WorkspaceID: wsID, Filename: "notes.odt", Size: int64(len(odt)), Body: bytes.NewReader(odt)}); err != nil {
		t.Errorf("valid odt: %v", err)
	}
	_, err = hd.SaveUpload(e.Ctx, st, nil, hd.Upload{
		WorkspaceID: wsID, Filename: "notes.ods", Size: int64(len(odt)), Body: bytes.NewReader(odt)})
	if !errors.Is(err, hd.ErrOfficePackageInvalid) {
		t.Errorf("odt accepted as ods: %v", err)
	}
}

// withEntry returns files with the named entry's data rewritten; its raw flag
// is reset so the package is rebuilt with a streamed data descriptor.
func withEntry(files []testFile, name string, fn func([]byte) []byte) []testFile {
	out := cloneFiles(files)
	for i := range out {
		if out[i].name == name {
			out[i].data = fn(out[i].data)
			out[i].raw = false
		}
	}
	return out
}

func cloneFiles(files []testFile) []testFile {
	out := make([]testFile, len(files))
	copy(out, files)
	return out
}

// corruptEntryData flips bytes inside a deflated entry's data, past the
// three-byte deflate header, without touching the central directory layout.
func corruptEntryData(t *testing.T, pkg []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		off, err := f.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		out := append([]byte{}, pkg...)
		out[int(off)+4] ^= 0xff
		out[int(off)+5] ^= 0xff
		return out
	}
	t.Fatal("entry not found")
	return nil
}
