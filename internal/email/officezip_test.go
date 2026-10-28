package email

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/useopendesk/opendesk/internal/crypto"
)

// minimalDocx builds a smallest valid OOXML package for an inbound mail.
func minimalDocx(t *testing.T) []byte {
	t.Helper()
	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`
	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body/></w:document>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct{ name, data string }{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rels},
		{"word/document.xml", doc},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f.data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// plainZip is an archive with a program inside, no office package structure.
func plainZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("evil.exe")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("MZ\x90\x00\x03\x00\x00\x00"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInboundOfficePackagesLabelled(t *testing.T) {
	w := newWorld(t)
	m := w.mailbox(InboundForwarding, OutboundSystem, nil)
	good := minimalDocx(t)
	fake := plainZip(t)
	part := func(filename string, data []byte) string {
		return "--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n" +
			"Content-Disposition: attachment; filename=\"" + filename + "\"\r\n\r\n" +
			base64.StdEncoding.EncodeToString(data) + "\r\n"
	}
	raw := "From: " + customer() + "\r\nTo: " + w.forwardAddr(m, "") + "\r\nSubject: Docs\r\nMessage-ID: <pkg" +
		crypto.RandomString(6) + "@a.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n" +
		part("report.docx", good) +
		part("invoice.docx", fake) +
		"--b--\r\n"
	msg := w.messageFor(w.receive(m, raw))

	got := map[string]string{}
	rows, err := w.d.DB.Pool.Query(w.ctx,
		`SELECT filename, content_type FROM attachments WHERE message_id = $1 ORDER BY filename`, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, ct string
		if err := rows.Scan(&name, &ct); err != nil {
			t.Fatal(err)
		}
		got[name] = ct
	}
	if !strings.Contains(got["report.docx"], "wordprocessingml") {
		t.Errorf("valid docx stored as %q", got["report.docx"])
	}
	if got["invoice.docx"] != "application/zip" {
		t.Errorf("plain ZIP named docx stored as %q, want application/zip", got["invoice.docx"])
	}
}
