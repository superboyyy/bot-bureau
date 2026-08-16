package engine

import (
	"botbureau/backend/internal/config"

	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One file, two places: an original in the store for the UI to draw history from, and a copy in the
// recipient's workspace, which is the one the bot actually reads.
func TestUploadsDeliverIntoWorkspace(t *testing.T) {
	dir := t.TempDir()
	u := NewUploads(dir)
	ws := filepath.Join(dir, "workspaces", "bot")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	a, err := u.Put("报价单.pdf", "application/pdf", []byte("%PDF-1.4 hello"))
	if err != nil {
		t.Fatal(err)
	}

	// Identical content a second time costs no extra copy, and both refer to the same thing
	b, err := u.Put("报价单.pdf", "application/pdf", []byte("%PDF-1.4 hello"))
	if err != nil || b.ID != a.ID {
		t.Fatalf("identical content should dedupe: %v %v", a.ID, b.ID)
	}

	paths := u.Deliver(ws, []Attachment{a})
	if len(paths) != 1 || paths[0] != filepath.Join("inbox", "报价单.pdf") {
		t.Fatalf("wrong inbox path: %v", paths)
	}
	got, err := os.ReadFile(filepath.Join(ws, paths[0]))
	if err != nil || string(got) != "%PDF-1.4 hello" {
		t.Fatalf("the file did not arrive in the workspace: %v %q", err, got)
	}

	// Same name never overwrites: two screenshots both called screenshot.png are two different things
	c, _ := u.Put("报价单.pdf", "application/pdf", []byte("a different document"))
	paths = u.Deliver(ws, []Attachment{c})
	if len(paths) != 1 || paths[0] != filepath.Join("inbox", "报价单-2.pdf") {
		t.Fatalf("a colliding name should be given a suffix: %v", paths)
	}

	// The list has to state the path and the size: the model needs to know where to read
	desc := Describe([]Attachment{a}, []string{paths[0]})
	if !strings.Contains(desc, "inbox/") {
		t.Fatalf("the description should name the path: %q", desc)
	}
}

// A filename arrives from outside and gets joined into an inbox path.
func TestSafeNameCannotEscapeTheInbox(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "/etc/passwd", "..", ".", "", "a/b/c.txt", "..\\..\\win.ini"} {
		got := safeName(in)
		if strings.ContainsAny(got, `/\`) || got == ".." || got == "." || got == "" {
			t.Fatalf("safeName(%q) = %q, which is not a safe bare name", in, got)
		}
	}
	if got := safeName("  报价 单.pdf "); got != "报价 单.pdf" {
		t.Fatalf("an ordinary name should survive: %q", got)
	}
}

// Four image types can be shown to the model; anything else is a file, not a picture.
func TestIsImage(t *testing.T) {
	for _, m := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "IMAGE/PNG"} {
		if !(Attachment{MIME: m}).IsImage() {
			t.Fatalf("%s should count as an image", m)
		}
	}
	for _, m := range []string{"image/svg+xml", "image/tiff", "application/pdf", "text/plain", ""} {
		if (Attachment{MIME: m}).IsImage() {
			t.Fatalf("%s should not be sent as an image block", m)
		}
	}
}

// Three things happen when an attachment arrives: it lands in the workspace, the text names its path,
// and an image enters the context.
func TestReceiveFilesPlacesAndDescribes(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus()
	sched := NewScheduler(bus, filepath.Join(dir, "routines.json"))
	deps := newTestDeps(t, dir)
	deps.Uploads = NewUploads(dir)

	w, err := NewBotWorker(config.BotConfig{Name: "worker", Role: "test", Provider: "fake"}, bus, sched, dir, deps)
	if err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n fake pixels")
	img, err := deps.Uploads.Put("shot.png", "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := deps.Uploads.Put("notes.txt", "text/plain", []byte("some notes"))
	if err != nil {
		t.Fatal(err)
	}

	text, images := w.receiveFiles(Msg{Sender: "user", Chat: "dm", Respond: true,
		Content: "看看这个", Files: []Attachment{img, doc}})

	for _, want := range []string{"看看这个", "inbox/shot.png", "inbox/notes.txt"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}

	// The image enters the context and the text file does not: that one is to be read, not looked at
	if len(images) != 1 || images[0].MIME != "image/png" {
		t.Fatalf("exactly the image should become an image block: %+v", images)
	}
	for _, p := range []string{"inbox/shot.png", "inbox/notes.txt"} {
		if _, err := os.Stat(filepath.Join(w.workspace, p)); err != nil {
			t.Fatalf("%s never reached the workspace: %v", p, err)
		}
	}
}
