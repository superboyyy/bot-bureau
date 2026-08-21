package engine

import (
	"botbureau/backend/internal/i18n"

	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// A file the user attached in the composer.

// One file is stored twice, and each copy has its own job:

// - data/uploads/<content hash><ext> — what the UI needs to draw a thumbnail when scrolling back
// through history. The event stream carries metadata only (id, name, type, size); no bytes ever
// reach the event log, since base64 in there would blow the chat log apart after a few screenshots.
// - <recipient's workspace>/inbox/<name> — the copy the bot actually reads.

// The second one is the point. The user asked for "the current bot's workspace", which also happens to
// sit right with the permission rules: a file inside its own workspace is one it can read_file or cat
// without an approval. Kept only under data/uploads it would be on the engine's ground, out of the
// bot's reach, and the attachment would be decoration only the UI could see.

// Names come from the content hash, so pasting the same screenshot ten times costs one copy; a name
// that collides inside an inbox gets -2, -3 appended.

const (

	// The caps on one message: how many attachments, how large each, how large in total.
	// They are enforced here because the bytes travel as base64 inside JSON — the encoding alone adds a
	// third — and all of it ends up as one request body and one span of memory.
	MaxAttachments      = 8
	MaxAttachmentBytes  = 10 << 20
	MaxAttachmentsBytes = 25 << 20

	// SendJSONMax is the largest /api/send body. Files travel as base64 inside JSON, so the encoding
	// adds a third on top of MaxAttachmentsBytes; a little extra covers names, mime types, and the
	// rest of the message. The default 1MiB JSON cap is far too small for a screenshot.
	SendJSONMax = MaxAttachmentsBytes*4/3 + 1<<20

	inboxDir = "inbox"
)

// Attachment is one attachment's metadata. The bytes are not in it; they lie under data/uploads, keyed by ID.
type Attachment struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	MIME  string `json:"mime"`
	Bytes int64  `json:"bytes"`
}

// IsImage reports whether this attachment can be shown to the model directly.
// Four types only: the vision APIs on both the Anthropic and the OpenAI side accept just these, and
// claiming any other one buys nothing but a 400.
func (a Attachment) IsImage() bool {
	switch strings.ToLower(a.MIME) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Uploads is the store of originals, one per team.
type Uploads struct{ dir string }

func NewUploads(dataDir string) *Uploads { return &Uploads{dir: filepath.Join(dataDir, "uploads")} }

// Put takes a file in and returns its metadata. Identical content arriving again reuses what is there.
func (u *Uploads) Put(name, mimeType string, data []byte) (Attachment, error) {
	if len(data) == 0 {
		return Attachment{}, errors.New(i18n.T("The file is empty"))
	}
	if len(data) > MaxAttachmentBytes {
		return Attachment{}, fmt.Errorf(i18n.T("%s is larger than the %s limit for one attachment"),
			safeName(name), humanBytes(MaxAttachmentBytes))
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])[:20]
	a := Attachment{ID: id, Name: safeName(name), MIME: normalizeMIME(mimeType, name), Bytes: int64(len(data))}
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return Attachment{}, err
	}
	p := u.path(a)
	if _, err := os.Stat(p); err == nil {
		return a, nil // this exact content is already here
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return Attachment{}, err
	}
	return a, nil
}

// path locates the original in the store. The extension follows the file's name so that anyone looking
// through the directory can tell what things are.
func (u *Uploads) path(a Attachment) string {
	ext := strings.ToLower(filepath.Ext(a.Name))
	if len(ext) > 12 || strings.ContainsAny(ext, `/\`) {
		ext = ""
	}
	return filepath.Join(u.dir, a.ID+ext)
}

// Read fetches an original back. The id arrives over HTTP, so its shape is checked first: hexadecimal
// only, and anything carrying a path separator or .. is rejected here rather than joined into a ReadFile.
func (u *Uploads) Read(a Attachment) ([]byte, error) {
	if !validUploadID(a.ID) {
		return nil, errors.New(i18n.T("Invalid attachment id"))
	}
	return os.ReadFile(u.path(a))
}

// Find locates an original by id and returns its metadata and bytes. This is how the UI fetches an
// image when scrolling back through history.
func (u *Uploads) Find(id string) (Attachment, []byte, error) {
	if !validUploadID(id) {
		return Attachment{}, nil, errors.New(i18n.T("Invalid attachment id"))
	}
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return Attachment{}, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), id) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(u.dir, e.Name()))
		if err != nil {
			return Attachment{}, nil, err
		}
		return Attachment{
			ID: id, Name: e.Name(), Bytes: int64(len(raw)),
			MIME: normalizeMIME("", e.Name()),
		}, raw, nil
	}
	return Attachment{}, nil, fs.ErrNotExist
}

// Deliver places a batch of attachments into one member's inbox and returns their paths relative to
// that workspace, in the order given.

// A hard link is tried first: one original sent to three people in a group costs three directory
// entries this way and three full copies otherwise. When linking is impossible — a different
// filesystem, or anything else — it falls back to copying, because an attachment failing to arrive is
// far worse than some duplicated disk.
func (u *Uploads) Deliver(workspace string, files []Attachment) []string {
	if len(files) == 0 {
		return nil
	}
	box := filepath.Join(workspace, inboxDir)
	if err := os.MkdirAll(box, 0o755); err != nil {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		dst := freeName(box, f.Name)
		src := u.path(f)
		if err := os.Link(src, dst); err != nil {
			raw, rerr := os.ReadFile(src)
			if rerr != nil {
				continue
			}
			if os.WriteFile(dst, raw, 0o644) != nil {
				continue
			}
		}
		out = append(out, filepath.Join(inboxDir, filepath.Base(dst)))
	}
	return out
}

// Describe assembles the lines appended to the user's message, telling the model what arrived and where
// it is.

// Images are listed too, even though they also travel as image blocks: seeing a picture is not knowing
// what it is called on disk. Cropping it, converting it, or merely naming it in a reply all need the path.
func Describe(files []Attachment, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(i18n.T("\n\n[Attachments the user sent with this message, already saved in your workspace]\n"))
	for i, p := range paths {
		if i >= len(files) {
			break
		}
		fmt.Fprintf(&b, "- %s (%s, %s)\n", p, files[i].MIME, humanBytes(files[i].Bytes))
	}
	return strings.TrimRight(b.String(), "\n")
}

// safeName reduces a user-supplied filename to a safe bare name.
// It gets joined into an inbox path, so separators, .. and leading dots go here.
func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, name)
	name = strings.TrimLeft(name, ".")
	if name == "" || name == ".." {
		name = "file"
	}
	if r := []rune(name); len(r) > 80 {
		ext := filepath.Ext(name)
		name = string(r[:70]) + ext
	}
	return name
}

// freeName finds an unused name in the inbox: a.png, a-2.png, a-3.png, …
// Same name never overwrites: two screenshots both called screenshot.png are two different things.
func freeName(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; i < 1000; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
	}
	return p
}

// normalizeMIME settles the type. What the browser reported wins, since it has seen the file itself;
// some dragged-in files come through with an empty type, and those fall back to the extension.
func normalizeMIME(reported, name string) string {
	reported = strings.ToLower(strings.TrimSpace(strings.Split(reported, ";")[0]))
	if reported != "" && strings.Contains(reported, "/") {
		return reported
	}
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return strings.Split(t, ";")[0]
	}
	return "application/octet-stream"
}

func validUploadID(id string) bool {
	if len(id) != 20 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
