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

// 用户在聊天框里带上来的文件。
//
// 一份原件存两处，两处各有各的用途：
//
//   - data/uploads/<内容散列><后缀> —— 界面回看历史时要拿它渲染缩略图。事件流里只留元数据
//     （id / 文件名 / 类型 / 大小），正文一律不进 events.json——base64 塞进去，聊天记录会被
//     几张截图撑爆。
//   - <收件人工作目录>/inbox/<文件名> —— 机器人真正读到的那一份。
//
// 第二处是关键。用户说"存到当前 bot 工作目录"，这也正好和权限那套自洽：文件落在它自己的
// 工作目录里，它 read_file / bash 去读都不触发审批。要是只存在 data/uploads 下，
// 那是引擎的地盘，机器人碰不到，附件就成了一个只有界面看得见的装饰。
//
// 按内容散列命名，所以同一张图贴十次只占一份；文件名冲突时在收件箱里加 -2、-3。
//
// A file the user attached in the composer.
//
// One file is stored twice, and each copy has its own job:
//
//   - data/uploads/<content hash><ext> — what the UI needs to draw a thumbnail when scrolling back
//     through history. The event stream carries metadata only (id, name, type, size); no bytes ever
//     reach events.json, since base64 in there would blow the chat log apart after a few screenshots.
//   - <recipient's workspace>/inbox/<name> — the copy the bot actually reads.
//
// The second one is the point. The user asked for "the current bot's workspace", which also happens to
// sit right with the permission rules: a file inside its own workspace is one it can read_file or cat
// without an approval. Kept only under data/uploads it would be on the engine's ground, out of the
// bot's reach, and the attachment would be decoration only the UI could see.
//
// Names come from the content hash, so pasting the same screenshot ten times costs one copy; a name
// that collides inside an inbox gets -2, -3 appended.

const (
	// 一条消息最多带这么多个附件，单个和合计各有上限。
	// 拦在这里是因为正文走 JSON base64：编码本身要多占三分之一，而这些数字最终都要变成
	// 一次 HTTP 请求体和一段内存。
	//
	// The caps on one message: how many attachments, how large each, how large in total.
	// They are enforced here because the bytes travel as base64 inside JSON — the encoding alone adds a
	// third — and all of it ends up as one request body and one span of memory.
	MaxAttachments      = 8
	MaxAttachmentBytes  = 10 << 20
	MaxAttachmentsBytes = 25 << 20
	inboxDir            = "inbox"
)

// Attachment 是一份附件的元数据。正文不在里面：它躺在 data/uploads 下，用 ID 取。
// Attachment is one attachment's metadata. The bytes are not in it; they lie under data/uploads, keyed by ID.
type Attachment struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	MIME  string `json:"mime"`
	Bytes int64  `json:"bytes"`
}

// IsImage 判断这份附件能不能直接给模型看。
// 只认这四种：Anthropic 和 OpenAI 两边的 vision 接口都只收这几个，多报一种只会换来一个 400。
//
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

// Uploads 是原件仓库，一支团队共用一个。
// Uploads is the store of originals, one per team.
type Uploads struct{ dir string }

func NewUploads(dataDir string) *Uploads { return &Uploads{dir: filepath.Join(dataDir, "uploads")} }

// Put 收下一份文件并返回它的元数据。同样的内容再来一次就直接复用，不重复落盘。
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
		return a, nil // 同一份内容已经存过 / this exact content is already here
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return Attachment{}, err
	}
	return a, nil
}

// path 是原件在仓库里的位置。后缀跟着文件名走，方便人翻这个目录时认出是什么。
// path locates the original in the store. The extension follows the file's name so that anyone looking
// through the directory can tell what things are.
func (u *Uploads) path(a Attachment) string {
	ext := strings.ToLower(filepath.Ext(a.Name))
	if len(ext) > 12 || strings.ContainsAny(ext, `/\`) {
		ext = ""
	}
	return filepath.Join(u.dir, a.ID+ext)
}

// Read 取回原件。id 从 HTTP 请求里来，所以先验形状：只认十六进制，
// 任何带路径分隔符或 .. 的东西在这里就被挡掉，不会拼进 ReadFile。
//
// Read fetches an original back. The id arrives over HTTP, so its shape is checked first: hexadecimal
// only, and anything carrying a path separator or .. is rejected here rather than joined into a ReadFile.
func (u *Uploads) Read(a Attachment) ([]byte, error) {
	if !validUploadID(a.ID) {
		return nil, errors.New(i18n.T("Invalid attachment id"))
	}
	return os.ReadFile(u.path(a))
}

// Find 按 id 找一份原件，返回它的元数据和正文。界面回看历史时靠它取图。
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

// Deliver 把一批附件放进某位成员的收件箱，返回它在工作目录里的相对路径（顺序和入参一致）。
//
// 先试硬链接：同一份原件发给群里三个人，硬链接只多三个目录项，拷贝就是三份完整数据。
// 跨文件系统或者别的什么原因链不上，就老老实实拷一份——附件送不到，比多占点磁盘严重得多。
//
// Deliver places a batch of attachments into one member's inbox and returns their paths relative to
// that workspace, in the order given.
//
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

// Describe 拼出附在用户消息末尾的那几行，告诉模型东西在哪、是什么。
//
// 图片也照样列出来，哪怕它同时以图片块喂了过去：模型看得见图，不等于知道这张图在磁盘上叫什么。
// 要它去裁剪、转换、或者只是把文件名写进回复，都得先知道路径。
//
// Describe assembles the lines appended to the user's message, telling the model what arrived and where
// it is.
//
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

// safeName 把用户给的文件名收成一个安全的裸文件名。
// 它会被拼进收件箱的路径，所以路径分隔符、.. 和前导点都要在这里去掉。
//
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

// freeName 在收件箱里找一个没被占用的名字：a.png、a-2.png、a-3.png…
// 同名不覆盖：两张都叫 screenshot.png 的截图是两件不同的东西。
//
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

// normalizeMIME 定下类型。浏览器给的那个优先，因为它见过文件本身；
// 拖进来的某些文件浏览器会给空串，那就按后缀猜。
//
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
