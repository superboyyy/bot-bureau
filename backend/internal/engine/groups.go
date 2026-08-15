package engine

import (
	"botbureau/backend/internal/i18n"

	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// 群聊。默认群的 id 固定是 "group"，其余是 g_xxxxxxxx。
// 成员是 bot 名字的列表，列表第一个就是不点名时的默认收件人，所以顺序有意义。
//
// A group chat. The default group's id is always "group"; the rest are g_xxxxxxxx.
// Members are bot names, and the first one is the default recipient when nobody is mentioned —
// so the order carries meaning.
type Group struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Avatar  string   `json:"avatar"`
	Members []string `json:"members"`
}

// 会话 id 是不是群聊。私聊是 dm:<bot>，全局事件的 chat 为空。
// Whether a conversation id denotes a group chat. DMs are dm:<bot>, and global events carry an empty chat.
// IsGroupChat 判断会话 id 是不是群聊。私聊是 dm:<bot>，全局事件的 chat 为空。
// IsGroupChat reports whether a conversation id denotes a group chat. DMs are dm:<bot>, and global
// events carry an empty chat.
func IsGroupChat(chat string) bool {
	return chat == "group" || strings.HasPrefix(chat, "g_")
}

func (b *Bus) groupLocked(id string) *Group {
	if id == "" {
		id = "group"
	}
	for _, g := range b.groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// defaultGroupLocked 取默认群，没有就建一个并放在最前。
// 默认群必须始终存在：老配置、Telegram 桥和不带 chat 的投递都落在它身上。
//
// defaultGroupLocked returns the default group, creating it at the front of the list if absent.
// It must always exist: legacy configs, the Telegram bridge, and chat-less delivery all land there.
func (b *Bus) defaultGroupLocked() *Group {
	if g := b.groupLocked("group"); g != nil {
		return g
	}
	g := &Group{ID: "group"}
	b.groups = append([]*Group{g}, b.groups...)
	return g
}

// Groups 返回可见的群聊。
//
// 默认群没有成员时不露面。它是隐式存在的东西，只有真的有人在里面才有意义——
// 摆一个"群聊 · 0 名成员"只会让人以为里面有人。往里加第一个人时它自然出现
// （见 SetGroupMemberIn 的按需创建）。
//
// 自建的群不受此限：那是用户明确建出来的，哪怕暂时没人也得留着，否则刚建完就消失了。
//
// Groups returns the visible group chats.
//
// The default group stays hidden while it has no members. It exists implicitly and only means
// something once somebody is actually in it; a "Group chat · 0 members" entry merely suggests
// otherwise. Adding the first member brings it back (see the on-demand creation in SetGroupMemberIn).
//
// User-created groups are exempt: those were made deliberately and must survive being empty, or one
// would vanish the moment it was created.
func (b *Bus) Groups() []Group {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Group, 0, len(b.groups))
	for _, g := range b.groups {
		if g.ID == "group" && len(g.Members) == 0 {
			continue
		}
		cp := *g
		if cp.Members == nil {
			cp.Members = []string{}
		}
		out = append(out, cp)
	}
	return out
}

// GroupMembersOf 返回该群当前真实存在的成员（顺序保留）。
// 会顺手滤掉已被删除的 bot——群成员表是独立存盘的，可能比 bots.yaml 旧。
//
// GroupMembersOf returns the group's members that still exist, preserving order.
// Deleted bots are filtered out: the membership file is persisted separately and can lag bots.yaml.
func (b *Bus) GroupMembersOf(chat string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groupLocked(chat)
	if g == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range g.Members {
		if seen[n] {
			continue
		}
		if _, ok := b.bots[n]; ok {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out
}

func (b *Bus) IsGroupMemberOf(chat, name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groupLocked(chat)
	if g == nil {
		return false
	}
	for _, n := range g.Members {
		if n == name {
			return true
		}
	}
	return false
}

// DefaultGroupMemberOf 是不点名时的收件人：成员列表里的第一个。
// DefaultGroupMemberOf is the recipient when nobody is mentioned: the first member on the list.
func (b *Bus) DefaultGroupMemberOf(chat string) string {
	ms := b.GroupMembersOf(chat)
	if len(ms) == 0 {
		return ""
	}
	return ms[0]
}

// MentionedBotsIn 找出这段话点到了哪些 bot。
//
// id 和显示名都算。@提及 id 是内部的稳定键（工作目录、任务归属、历史消息都认它），
// 而用户在界面上看到、嘴里叫的是显示名——只认 id 的话，把 bot 改名叫「小文」之后
// 群里喊「小文」它不理，而用户根本不知道自己还得去记那个 id。
//
// MentionedBotsIn finds which bots a message calls on.
//
// Both the id and the display name count. The @mention id is the internal stable key (workspaces, task
// ownership and past messages all reference it), but what the user sees on screen and says out loud is
// the display name — matching the id alone means a bot renamed to "Wren" ignores anyone calling for
// Wren, while the user has no idea they were supposed to remember an id at all.
func (b *Bus) MentionedBotsIn(chat, text string) []string {
	var out []string
	for _, n := range b.GroupMembersOf(chat) {
		if containsMention(text, n) || containsMention(text, b.displayNameOf(n)) {
			out = append(out, n)
		}
	}
	return out
}

// displayNameOf 返回该 bot 的显示名；没另设显示名时返回空串（免得和 id 重复匹配一次）。
// displayNameOf returns a bot's display name, or empty when it has none (so the id is not matched twice).
func (b *Bus) displayNameOf(name string) string {
	b.mu.Lock()
	w := b.bots[name]
	b.mu.Unlock()
	if w == nil {
		return ""
	}
	if title := w.Cfg.Title(); title != name {
		return title
	}
	return ""
}

// SetGroupMemberIn 把某个 bot 拉进/移出群聊。
// ok 表示这个 bot（和这个群）确实存在，changed 表示成员表真的动了——
// 已经在群里的人被再"拉"一次不算变化，调用方靠它决定要不要播报"XX 被拉进群聊"。
//
// SetGroupMemberIn adds a bot to a group chat or removes it from one.
// ok reports that the bot (and the group) exists; changed reports that the membership list actually
// moved — re-"adding" someone who is already in is not a change. Callers use it to decide whether to
// announce "X was added to the group chat".
func (b *Bus) SetGroupMemberIn(chat, name string, in bool) (ok, changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.bots[name]; !exists {
		return false, false
	}
	g := b.groupLocked(chat)
	if g == nil {
		// 默认群按需创建：还没写过成员文件时它并不存在，但"把某人加进群聊"这个动作
		// 本身就该把它带出来，否则第一次拉人会静默失败。自建群不适用——那必须先建后加。
		//
		// The default group is created on demand: before any membership file is written it does not
		// exist, yet "add someone to the group chat" should bring it into being, or the very first add
		// fails silently. Not so for user-created groups: those must exist before anyone joins.
		if chat != "" && chat != "group" {
			return false, false
		}
		g = b.defaultGroupLocked()
	}
	if in {
		for _, n := range g.Members {
			if n == name {
				return true, false
			}
		}
		g.Members = append(g.Members, name)
	} else {
		kept := g.Members[:0]
		for _, n := range g.Members {
			if n != name {
				kept = append(kept, n)
			}
		}
		if len(kept) == len(g.Members) {
			return true, false
		}
		g.Members = kept
	}
	b.saveGroupsLocked()
	return true, true
}

func (b *Bus) CreateGroup(title, avatar string, members []string) (*Group, error) {
	title = strings.TrimSpace(title)
	b.mu.Lock()
	defer b.mu.Unlock()
	id := newGroupID()
	for b.groupLocked(id) != nil {
		id = newGroupID()
	}
	g := &Group{ID: id, Title: title, Avatar: avatar, Members: b.filterBotsLocked(members)}
	b.groups = append(b.groups, g)
	b.saveGroupsLocked()
	cp := *g
	return &cp, nil
}

func (b *Bus) UpdateGroup(id string, title, avatar *string, members *[]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groupLocked(id)
	if g == nil {
		return errors.New(i18n.T("No such group chat"))
	}
	if title != nil {
		g.Title = strings.TrimSpace(*title)
	}
	if avatar != nil {
		g.Avatar = *avatar
	}
	if members != nil {
		g.Members = b.filterBotsLocked(*members)
	}
	b.saveGroupsLocked()
	return nil
}

func (b *Bus) DeleteGroup(id string) error {
	if id == "" || id == "group" {
		return errors.New(i18n.T("The default group chat cannot be deleted"))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.groupLocked(id) == nil {
		return errors.New(i18n.T("No such group chat"))
	}
	kept := b.groups[:0]
	for _, g := range b.groups {
		if g.ID != id {
			kept = append(kept, g)
		}
	}
	b.groups = kept
	b.saveGroupsLocked()
	return nil
}

// PostGroupTo 往某个群发一条消息：先进事件流，再投给除发送者外的所有成员。
// 只有 respondTargets 里的成员会被要求回复，其余人只是"听见了"，写进上下文当背景。
// 这是分工不打架的关键——否则一句话会把整个群都激活。
//
// PostGroupTo posts one message to a group: it enters the event stream, then goes to every member
// except the sender. Only members in respondTargets are asked to reply; the rest merely overhear it
// as context. This is what keeps the division of labor from collapsing — otherwise one message
// would wake the entire group at once.
func (b *Bus) PostGroupTo(chat, sender, text string, respondTargets []string) {
	b.PostGroupQuoted(chat, sender, text, respondTargets, nil)
}

// PostGroupUser 是用户往群里发的一条消息：可能引用了谁，可能带着附件。
// 附件每位收件人各得一份——它是发到这个房间里的，不是交给某一个人的。
//
// PostGroupUser is one message the user posts to a group: possibly quoting someone, possibly carrying
// attachments. Every recipient gets its own copy of those: an attachment is posted to the room rather
// than handed to one person.
func (b *Bus) PostGroupUser(chat, text string, respondTargets []string, q *Quote, files []Attachment) Event {
	return b.postGroup(chat, "user", text, respondTargets, q, files)
}

// PostGroupQuoted 同 PostGroupTo，但这条发言引用了群里更早的某条消息。
// 引用两边都要送到：写进事件（界面画成一条引文）之外，还要随消息进每个收件人的上下文，
// 否则模型只看得到孤零零一句"那就照第二个方案办"，不知道第二个方案是谁说的哪一句。
//
// PostGroupQuoted is PostGroupTo for a message that quotes an earlier one in the group.
// The quotation has to reach both sides: it goes into the event (where the UI draws it as a
// quotation) and into every recipient's context, or the model sees a bare "go with the second one"
// without knowing whose second one, or where.
func (b *Bus) PostGroupQuoted(chat, sender, text string, respondTargets []string, q *Quote) Event {
	return b.postGroup(chat, sender, text, respondTargets, q, nil)
}

func (b *Bus) postGroup(chat, sender, text string, respondTargets []string, q *Quote, files []Attachment) Event {
	if chat == "" {
		chat = "group"
	}
	ev := b.Emit("msg", chat, sender, text, MsgExtra(q, files))
	targets := map[string]bool{}
	for _, t := range respondTargets {
		targets[t] = true
	}
	// 用户在群里一个人都没点名时，他指定的目录是说给全群的。
	//
	// 这里重算一次点名，而不是看 respondTargets：调用方在没人被点名时会填上默认收件人，
	// 到这儿就分不出"用户点了默认那位"和"用户谁都没点、活落给了默认那位"了。
	// 而这两种情况下这句授权该给谁，答案正好相反。
	//
	// When the user named nobody in the group, the directories they named are named to everyone.
	//
	// Mentions are recomputed here rather than read off respondTargets: with nobody named the caller
	// fills in the default recipient, and by this point "the user named the default member" and "the
	// user named no one and the job fell to the default member" are indistinguishable — yet those two
	// cases want opposite answers about who the grant reaches.
	broadcast := sender == "user" && len(b.MentionedBotsIn(chat, text)) == 0
	for _, name := range b.GroupMembersOf(chat) {
		if name == sender {
			continue
		}
		b.DeliverMsg(name, Msg{
			Sender: sender, Content: text, Chat: chat, Respond: targets[name],
			ID: EventID(ev), Quote: q, Files: files,
			GrantRoots: sender == "user" && (targets[name] || broadcast),
		})
	}
	return ev
}

// LoadGroups 装载群聊：优先读新的 groups.json；没有就把旧的单群 group.json 迁移过来，
// 再把设置里存的默认群名字/头像补上（那是多群聊之前的存法）。
//
// LoadGroups loads the group chats: it prefers the new groups.json, falls back to migrating the old
// single-group group.json, and finally fills in the default group's name/avatar from settings
// (where they lived before multiple groups existed).
func (b *Bus) LoadGroups(path, legacyPath string, title, avatar string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.groupsPath = path
	if b.loadGroupsFileLocked(path) {
		b.applyDefaultMetaLocked(title, avatar)
		return
	}
	if legacyPath != "" && b.loadLegacyMembersLocked(legacyPath) {
		b.applyDefaultMetaLocked(title, avatar)
		b.saveGroupsLocked()
		return
	}
	// 两个文件都没有 = 从没存过成员名单。这时把已注册的 bot 全部放进默认群：
	// 老版本里群成员就是"所有人"，升级上来的用户不该发现自己的群突然空了。
	// 全新安装走不到这一步——那时一个 bot 都还没注册。
	//
	// Neither file exists, so membership has never been persisted. Put every registered bot into the
	// default group: in older versions the group was everyone, and someone upgrading must not find
	// their group suddenly empty. A fresh install never reaches this with anything to add — no bot is
	// registered yet.
	for _, name := range b.order {
		b.addToDefaultLocked(name)
	}
	b.applyDefaultMetaLocked(title, avatar)
}

func (b *Bus) loadGroupsFileLocked(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var wrap struct {
		Groups []Group `json:"groups"`
	}
	if json.Unmarshal(raw, &wrap) != nil || len(wrap.Groups) == 0 {
		return false
	}
	b.groupsFile = true
	b.groups = nil
	for i := range wrap.Groups {
		g := wrap.Groups[i]
		if g.ID == "" {
			continue
		}
		g.Members = b.filterBotsLocked(g.Members)
		cp := g
		b.groups = append(b.groups, &cp)
	}
	b.defaultGroupLocked()
	return true
}

func (b *Bus) loadLegacyMembersLocked(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var f struct {
		Members []string `json:"members"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return false
	}
	b.groupsFile = true
	g := b.defaultGroupLocked()
	g.Members = b.filterBotsLocked(f.Members)
	return true
}

func (b *Bus) applyDefaultMetaLocked(title, avatar string) {
	g := b.defaultGroupLocked()
	if g.Title == "" {
		g.Title = title
	}
	if g.Avatar == "" {
		g.Avatar = avatar
	}
}

// saveGroupsLocked 存盘。路径还是老的 group.json 时按旧格式写，
// 免得降级回旧版本后群成员全丢。
//
// saveGroupsLocked persists the groups. When the path is still the old group.json it writes the old
// format, so downgrading to a previous version does not lose the membership list.
func (b *Bus) saveGroupsLocked() {
	if b.groupsPath == "" {
		return
	}
	b.groupsFile = true
	if strings.HasSuffix(b.groupsPath, "group.json") {
		g := b.defaultGroupLocked()
		out, _ := json.MarshalIndent(map[string]any{"members": g.Members}, "", "  ")
		_ = os.MkdirAll(filepath.Dir(b.groupsPath), 0o755)
		_ = os.WriteFile(b.groupsPath, out, 0o644)
		return
	}
	groups := make([]Group, 0, len(b.groups))
	for _, g := range b.groups {
		cp := *g
		if cp.Members == nil {
			cp.Members = []string{}
		}
		groups = append(groups, cp)
	}
	out, _ := json.MarshalIndent(map[string]any{"groups": groups}, "", "  ")
	_ = os.MkdirAll(filepath.Dir(b.groupsPath), 0o755)
	_ = os.WriteFile(b.groupsPath, out, 0o644)
}

// filterBotsLocked 去重并剔掉不存在的 bot。
// filterBotsLocked de-duplicates and drops names that no longer exist.
func (b *Bus) filterBotsLocked(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		if _, ok := b.bots[n]; ok {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out
}

func (b *Bus) addToDefaultLocked(name string) {
	g := b.defaultGroupLocked()
	for _, n := range g.Members {
		if n == name {
			return
		}
	}
	g.Members = append(g.Members, name)
}

// removeFromAllGroupsLocked 删除 bot 时把它从所有群里摘掉，不留悬空成员。
// removeFromAllGroupsLocked strips a deleted bot from every group so no dangling member is left.
func (b *Bus) removeFromAllGroupsLocked(name string) {
	for _, g := range b.groups {
		kept := g.Members[:0]
		for _, n := range g.Members {
			if n != name {
				kept = append(kept, n)
			}
		}
		g.Members = kept
	}
}

func newGroupID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return "g_" + hex.EncodeToString(buf[:])
}
