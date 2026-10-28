package hd

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"

	"github.com/useopendesk/opendesk/internal/httpx"
)

// FileType is a kind of file a widget may take (contracts/api.md "Widget"): its
// extensions, and the bytes a file of that kind starts with.
type FileType struct {
	ID         string   `json:"id"`
	Group      string   `json:"group" enum:"image,document,spreadsheet,presentation,media,archive"`
	Extensions []string `json:"extensions"`
	match      func(head []byte) bool
}

var fileTypes = []FileType{
	{ID: "png", Group: "image", Extensions: []string{".png"}, match: prefix("\x89PNG\r\n\x1a\n")},
	{ID: "jpg", Group: "image", Extensions: []string{".jpg", ".jpeg"}, match: prefix("\xff\xd8\xff")},
	{ID: "gif", Group: "image", Extensions: []string{".gif"}, match: prefix("GIF87a", "GIF89a")},
	{ID: "webp", Group: "image", Extensions: []string{".webp"}, match: riff("WEBP")},
	{ID: "heic", Group: "image", Extensions: []string{".heic", ".heif"}, match: ftyp("heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1")},
	{ID: "pdf", Group: "document", Extensions: []string{".pdf"}, match: prefix("%PDF-")},
	{ID: "txt", Group: "document", Extensions: []string{".txt"}, match: plainText},
	{ID: "csv", Group: "document", Extensions: []string{".csv"}, match: plainText},
	{ID: "rtf", Group: "document", Extensions: []string{".rtf"}, match: prefix(`{\rtf`)},
	{ID: "doc", Group: "document", Extensions: []string{".doc"}, match: prefix(ole)},
	// The ZIP-headed office formats pass the first-bytes gate on the prefix;
	// SaveUpload then checks the package structure the extension promises
	// (VerifyOfficePackage), so a plain archive cannot borrow the name.
	{ID: "docx", Group: "document", Extensions: []string{".docx"}, match: prefix(zip)},
	{ID: "odt", Group: "document", Extensions: []string{".odt"}, match: prefix(zip)},
	{ID: "xls", Group: "spreadsheet", Extensions: []string{".xls"}, match: prefix(ole)},
	{ID: "xlsx", Group: "spreadsheet", Extensions: []string{".xlsx"}, match: prefix(zip)},
	{ID: "ods", Group: "spreadsheet", Extensions: []string{".ods"}, match: prefix(zip)},
	{ID: "ppt", Group: "presentation", Extensions: []string{".ppt"}, match: prefix(ole)},
	{ID: "pptx", Group: "presentation", Extensions: []string{".pptx"}, match: prefix(zip)},
	{ID: "odp", Group: "presentation", Extensions: []string{".odp"}, match: prefix(zip)},
	{ID: "mp4", Group: "media", Extensions: []string{".mp4", ".m4v"}, match: ftyp()},
	{ID: "mov", Group: "media", Extensions: []string{".mov"}, match: ftyp()},
	{ID: "mp3", Group: "media", Extensions: []string{".mp3"}, match: mp3},
	{ID: "m4a", Group: "media", Extensions: []string{".m4a"}, match: ftyp()},
	{ID: "wav", Group: "media", Extensions: []string{".wav"}, match: riff("WAVE")},
	{ID: "webm", Group: "media", Extensions: []string{".webm"}, match: prefix("\x1a\x45\xdf\xa3")},
	{ID: "ogg", Group: "media", Extensions: []string{".ogg", ".oga", ".opus"}, match: prefix("OggS")},
	{ID: "zip", Group: "archive", Extensions: []string{".zip"}, match: prefix(zip)},
}

const (
	ole = "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"
	zip = "PK\x03\x04"
)

func prefix(magic ...string) func([]byte) bool {
	return func(head []byte) bool {
		return slices.ContainsFunc(magic, func(m string) bool { return bytes.HasPrefix(head, []byte(m)) })
	}
}

func riff(kind string) func([]byte) bool {
	return func(head []byte) bool {
		return len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == kind
	}
}

// ftyp matches an ISO base media file (MP4, QuickTime, HEIF); with brands, only those.
func ftyp(brands ...string) func([]byte) bool {
	return func(head []byte) bool {
		if len(head) < 12 || string(head[4:8]) != "ftyp" {
			return false
		}
		return len(brands) == 0 || slices.Contains(brands, string(head[8:12]))
	}
}

func mp3(head []byte) bool {
	return bytes.HasPrefix(head, []byte("ID3")) || (len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0)
}

// plainText is text in any 8-bit encoding (UTF-8, Windows-1252, Latin-1) without NUL or
// other control characters but tab, line breaks, form feed and the DOS end of file, or
// UTF-16 with its byte order mark.
func plainText(head []byte) bool {
	if bytes.HasPrefix(head, []byte("\xff\xfe")) || bytes.HasPrefix(head, []byte("\xfe\xff")) {
		return len(head) > 2
	}
	if len(head) == 0 {
		return false
	}
	for _, b := range head {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' && b != 0x1a || b == 0x7f {
			return false
		}
	}
	return true
}

// VoiceFileTypes are the types a browser records a voice message in: WebM (Chrome,
// Edge), Ogg (Firefox) and MP4 audio (Safari).
var VoiceFileTypes = []string{"webm", "ogg", "m4a"}

// IsVoice reports whether a file is a voice recording: one of VoiceFileTypes by name and
// bytes, holding sound and no picture.
func IsVoice(head []byte, filename string) bool {
	return FileTypeAllowed(head, filename, VoiceFileTypes) && strings.HasPrefix(DetectType(head, filename), "audio/")
}

// VoiceMaxBytes is the largest recording of seconds a widget takes: 256 kbit/s, twice
// what browsers record speech at and four times what the widget asks for, plus 64 KB
// for the container.
func VoiceMaxBytes(seconds int) int64 { return int64(seconds)*32*1024 + 64*1024 }

// FileTypes is the catalog a widget picks from.
func FileTypes() []FileType { return fileTypes }

// FileTypeExtensions lists the extensions of the given types in catalog order;
// unknown ids are skipped.
func FileTypeExtensions(ids []string) []string {
	out := []string{}
	for _, f := range fileTypes {
		if slices.Contains(ids, f.ID) {
			out = append(out, f.Extensions...)
		}
	}
	return out
}

// CleanFileTypes checks a picked list against the catalog: known ids, deduplicated, in
// catalog order. An empty list takes any file but a program or a script.
func CleanFileTypes(ids []string) ([]string, error) {
	for _, id := range ids {
		if !slices.ContainsFunc(fileTypes, func(f FileType) bool { return f.ID == id }) {
			return nil, httpx.Validation("attachment_types has an unknown type: " + Truncate(id, 20))
		}
	}
	return knownFileTypes(ids), nil
}

// knownFileTypes keeps the ids of the catalog, deduplicated, in catalog order.
func knownFileTypes(ids []string) []string {
	out := []string{}
	for _, f := range fileTypes {
		if slices.Contains(ids, f.ID) {
			out = append(out, f.ID)
		}
	}
	return out
}

// FileTypeAllowed reports whether a file is one of the given types: its extension is
// one of theirs and its first bytes are what that type starts with. A program or a
// script is never allowed, whatever its name.
func FileTypeAllowed(head []byte, filename string, ids []string) bool {
	if len(head) == 0 || IsExecutable(head, filename) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(filename))
	for _, f := range fileTypes {
		if slices.Contains(ids, f.ID) && slices.Contains(f.Extensions, ext) && f.match(head) {
			return true
		}
	}
	return false
}
