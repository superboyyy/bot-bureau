package api

import (
	"botbureau/backend/internal/bridge"
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/engine"
	"botbureau/backend/internal/httpx"
	"botbureau/backend/internal/model"
	"context"
	"crypto/rand"
	"encoding/hex"

	"botbureau/backend/internal/i18n"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// App aggregates the runtime objects and owns persistence of bots.yaml.
type App struct {
	bus      *engine.Bus
	sched    *engine.Scheduler
	deps     *engine.TeamDeps
	tg       *bridge.TGBridge
	settings *config.Settings
	cfgs     []config.BotConfig
	cfgPath  string
	dataDir  string
	mu       sync.Mutex
}

func NewApp(bus *engine.Bus, sched *engine.Scheduler, deps *engine.TeamDeps, tg *bridge.TGBridge, settings *config.Settings, cfgs []config.BotConfig, cfgPath, dataDir string) *App {
	a := &App{bus: bus, sched: sched, deps: deps, tg: tg, settings: settings, cfgs: cfgs, cfgPath: cfgPath, dataDir: dataDir}
	// Bots enable catalog connectors from chat; persistence of bots.yaml lives here.
	deps.SubscribeMCP = a.subscribeBotMCP
	return a
}

// subscribeBotMCP appends server to the bot's mcp list, updates the live worker, and saves bots.yaml.
func (a *App) subscribeBotMCP(botName, server string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.cfgs {
		if a.cfgs[i].Name != botName {
			continue
		}
		for _, s := range a.cfgs[i].MCP {
			if s == server {
				if w := a.bus.Bot(botName); w != nil {
					w.Cfg.MCP = a.cfgs[i].MCP
					w.Toolbox().SetMCPServers(a.cfgs[i].MCP)
				}
				return nil
			}
		}
		a.cfgs[i].MCP = append(append([]string(nil), a.cfgs[i].MCP...), server)
		if w := a.bus.Bot(botName); w != nil {
			w.Cfg.MCP = a.cfgs[i].MCP
			w.Toolbox().SetMCPServers(a.cfgs[i].MCP)
		}
		if err := config.SaveBotConfigs(a.cfgPath, a.cfgs); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf(i18n.T("There is no bot named %s"), botName)
}

// A random instance id per start, published on /api/ping.

// LAN discovery returns "there is an engine at this address", and the client has to recognise which
// one is itself — the local engine advertises over mDNS too, or the UI would offer to pair you with
// yourself on every launch. The hostname will not do: one machine can run several engines with
// different data directories.

// Random rather than a hash of the pairing code: /api/ping is unauthenticated, and nothing derived
// from the pairing code belongs there. Telling "me" from "them" within one session is all that is
// needed, and a fresh value after a restart is harmless.
var instanceID = newInstanceID()

func newInstanceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}

var reservedNames = map[string]bool{"user": true, "scheduler": true, "system": true, "group": true}

// credentials packages the key store and the two subscription tokens into the resolver the model layer wants.
func (a *App) credentials() model.CredentialFunc {
	return model.Credentials(a.deps.KS, a.deps.XAI, a.deps.ChatGPT)
}

func (a *App) AddBot(cfg config.BotConfig) (*engine.BotWorker, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if !config.ValidBotName(cfg.Name) {
		return nil, errors.New(i18n.T("Name must be 1-24 lowercase letters/digits/-/_"))
	}
	if reservedNames[cfg.Name] || a.bus.Bot(cfg.Name) != nil {
		return nil, fmt.Errorf(i18n.T("The name %s is already taken"), cfg.Name)
	}
	if err := a.validateBot(&cfg); err != nil {
		return nil, err
	}
	w, err := engine.NewBotWorker(cfg, a.bus, a.sched, a.dataDir, a.deps)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cfgs = append(a.cfgs, cfg)
	err = config.SaveBotConfigs(a.cfgPath, a.cfgs)
	a.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf(i18n.T("Failed to save bots.yaml: %v"), err)
	}
	a.bus.Register(w)

	// A new bot joins no group automatically.

	// A group chat means "these people, on this piece of work", and who belongs there is the user's
	// call. Auto-joining a newcomer would let it read everything said in the room and be handed work,
	// none of which the user asked for. Adding it is one checkbox in the group's settings.
	w.Start()
	a.bus.Emit("refresh", "", "system", i18n.T("New member ")+cfg.Name+i18n.T(" joined the team"), nil)
	return w, nil
}

// UpdateBot replaces an existing bot's configuration. Its @mention id (Name) is fixed; everything else is
// overwritten wholesale. Saving restarts the bot with the new config — switching provider or model would
// invalidate the old conversation context anyway.
func (a *App) UpdateBot(cfg config.BotConfig) error {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if a.bus.Bot(cfg.Name) == nil {
		return fmt.Errorf(i18n.T("There is no bot named %s"), cfg.Name)
	}
	if err := a.validateBot(&cfg); err != nil {
		return err
	}

	// Build the replacement first: if the provider fails (e.g. a missing key), abort rather than tear down a working bot
	w, err := engine.NewBotWorker(cfg, a.bus, a.sched, a.dataDir, a.deps)
	if err != nil {
		return err
	}
	if old := a.bus.Replace(cfg.Name, w); old != nil {
		old.Cancel()
		w.RestoreFrom(old)
		old.Stop()
	}
	a.mu.Lock()
	for i := range a.cfgs {
		if a.cfgs[i].Name == cfg.Name {
			a.cfgs[i] = cfg
		}
	}
	err = config.SaveBotConfigs(a.cfgPath, a.cfgs)
	a.mu.Unlock()
	w.Start()
	if err != nil {
		return fmt.Errorf(i18n.T("Failed to save bots.yaml: %v"), err)
	}
	a.bus.Emit("refresh", "", "system", fmt.Sprintf(i18n.T("%s's settings were updated"), cfg.Title()), nil)
	return nil
}

// effortProviderID keeps hand-written/legacy configs usable when provider_id was not persisted yet.
// The UI always sends the catalog id, but old files only carried provider and base_url.
func effortProviderID(cfg config.BotConfig) string {
	base := strings.ToLower(strings.TrimSpace(cfg.BaseURL))
	if strings.Contains(base, "x.ai") {
		return "xai"
	}
	if strings.Contains(base, "kimi.com") {
		return "kimi-code"
	}
	if family := config.EffortProviderFamily(cfg.ProviderID); family != "" {
		return family
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "anthropic":
		return "anthropic"
	case "deepseek":
		return "deepseek"
	case "openai", "openai-compatible", "openai_compatible", "moonshot", "opencode", "opencode-go", "ollama", "custom":
		return "openai"
	default:
		return ""
	}
}

// validateBot normalizes and checks the fields shared by create and edit.
func (a *App) validateBot(cfg *config.BotConfig) error {
	cfg.Role = strings.TrimSpace(cfg.Role)
	if cfg.Role == "" {
		cfg.Role = i18n.T("assistant")
	}
	cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	if utf8.RuneCountInString(cfg.DisplayName) > config.DisplayNameMax {
		return fmt.Errorf(i18n.T("The display name may be at most %d characters"), config.DisplayNameMax)
	}
	if err := a.checkNameClash(*cfg); err != nil {
		return err
	}
	if !config.ValidEffort(cfg.Effort) {
		return errors.New(i18n.T("The reasoning effort must be none / minimal / low / medium / high / xhigh / max, or empty for the vendor default"))
	}

	// The tier has to be one this concrete model actually takes: handing it an unknown one is an inscrutable 400.
	providerID := effortProviderID(*cfg)
	if !model.ReasoningEffortSupported(context.Background(), providerID, cfg.Model, cfg.Effort) {
		return fmt.Errorf(i18n.T("%s does not offer the %s reasoning effort"), providerID, cfg.Effort)
	}
	if cfg.Permission != "" && !config.ValidPerm(cfg.Permission) {
		return errors.New(i18n.T("The permission tier must be ask / edit / auto / full, or empty to follow the global setting"))
	}
	if !config.ValidAvatar(cfg.Avatar) {
		return errors.New(i18n.T("The avatar must be a #rrggbb color or a small png/jpeg/webp image"))
	}

	// Extra instructions ride in the system prompt of every single request, so an overlong one burns
	// context forever — cap it
	cfg.Prompt = strings.TrimSpace(cfg.Prompt)
	if len(cfg.Prompt) > config.PromptMax {
		return fmt.Errorf(i18n.T("The role instructions may be at most %d characters"), config.PromptMax)
	}
	for _, srv := range cfg.MCP {
		if !a.deps.MCP.Has(srv) {
			return fmt.Errorf(i18n.T("There is no plugin named %s"), srv)
		}
	}
	return nil
}

// checkNameClash keeps two members from wearing the same name.

// With ids hidden, the display name is the user's only handle, and calling someone in a group matches
// both the id and the display name (see Bus.MentionedBotsIn): two "Wu Min"s mean one shout and the
// same job done twice, while the two rows in the conversation list look identical and the user cannot
// even tell which pair collided. Say so at the point of naming and let them pick another — renaming
// costs nothing next to working out why a task ran twice.
func (a *App) checkNameClash(cfg config.BotConfig) error {
	name := strings.TrimSpace(cfg.DisplayName)
	if name == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.cfgs {
		if c.Name == cfg.Name {
			continue // skip the bot being edited
		}

		// Ids count too: a display name colliding with someone else's id calls on both just the same
		if strings.EqualFold(strings.TrimSpace(c.DisplayName), name) || strings.EqualFold(c.Name, name) {
			return fmt.Errorf(i18n.T("Another member is already called %s — pick a different name"), name)
		}
	}
	return nil
}

// RemoveBot takes a bot off the team. The team may be emptied — that is exactly where a fresh install
// starts, and the client shows onboarding there.

// purge decides what happens to this member's memory, work files, and DM history, chosen by the user
// in the removal dialog: true deletes all of them along with the member, false leaves the DM history
// intact and renames the workspace into an archive under data/workspaces, which settings then lists
// under "Former members" for viewing or permanent deletion. Keeping is the default because those files
// are what they produced, and removing a member should not quietly destroy them — but since an id is a
// one-shot random string that nobody will ever inherit, keeping only makes sense alongside somewhere
// to see and clear the archives; otherwise it just piles unclaimed junk into data/.

// Either way, routines assigned to them are revoked (see Scheduler.RemoveByBot).

// The returned string is "the removal went through, but you should know something" — files that would
// not delete, say. That belongs neither in error (which would claim the member is still here) nor in
// silence, so it travels back on its own and surfaces as a toast in the client.
func (a *App) RemoveBot(name string, purge bool) (string, error) {
	if a.bus.Bot(name) == nil {
		return "", fmt.Errorf(i18n.T("No bot named %s exists"), name)
	}
	w := a.bus.Unregister(name)
	if w == nil {
		return "", fmt.Errorf(i18n.T("No bot named %s exists"), name)
	}

	// Stop first, then touch the directory: Stop writes the session snapshot back into the workspace,
	// so the rename or delete has to come after it
	w.Stop()
	a.sched.RemoveByBot(name)
	a.mu.Lock()
	kept := a.cfgs[:0]
	for _, c := range a.cfgs {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	a.cfgs = kept
	_ = config.SaveBotConfigs(a.cfgPath, a.cfgs)
	a.mu.Unlock()

	// The pin leaves with the conversation: what would stay is an id with no row behind it, and since
	// ids never surface the user could neither see it nor take it back.
	a.settings.SetPinned("dm:"+name, false)

	// The chat log goes only when "delete their data as well" was ticked, and otherwise stays, on the
	// same footing as the memory and the work files.

	// What stays is unreachable from the UI afterwards (the row is gone), knowingly so: like the
	// workspace it is an archive rather than something still to be scrolled through. Deleting it without
	// the tick would be deleting what the user explicitly asked to keep.
	if purge {
		a.bus.DeleteChat("dm:" + name)
	}
	warning := a.settleWorkspace(w.Cfg, purge)
	a.bus.Emit("refresh", "", "system", w.Cfg.Title()+i18n.T(" was removed from the team"), nil)
	return warning, nil
}

// settleWorkspace disposes of the workspace as the user chose and returns a line to pass on (empty
// when all went well).

// A workspace that will not budge is not a failed removal: the member is already off the team, and
// returning an error here would say nothing happened at all. A failed delete falls back to archiving —
// leaving a directory that is neither gone nor suffixed .removed- would hide it from "Former
// members", stranding the user with no way to finish the job. Better a visible archive and a note.
func (a *App) settleWorkspace(cfg config.BotConfig, purge bool) string {
	if purge {
		err := engine.PurgeWorkspace(a.dataDir, cfg.Name)
		if err == nil {
			return ""
		}
		if _, arch := engine.ArchiveWorkspace(a.dataDir, cfg); arch == nil {
			return fmt.Sprintf(i18n.T("%s's files could not be deleted, so they were kept under Former members: %v"), cfg.Title(), err)
		}
		return fmt.Sprintf(i18n.T("Could not tidy up %s's files: %v"), cfg.Title(), err)
	}
	if _, err := engine.ArchiveWorkspace(a.dataDir, cfg); err != nil {
		return fmt.Sprintf(i18n.T("Could not tidy up %s's files: %v"), cfg.Title(), err)
	}
	return ""
}

// chatExists reports whether a conversation id currently names a real conversation.
// Groups are judged by the visible list: an empty default group never shows up in it, so there is
// nothing there to pin.
func (a *App) chatExists(chat string) bool {
	if name, ok := strings.CutPrefix(chat, "dm:"); ok {
		return a.bus.Bot(name) != nil
	}
	for _, g := range a.bus.Groups() {
		if g.ID == chat {
			return true
		}
	}
	return false
}

// quotedIn resolves the message being quoted. It has to come from the same conversation: an id from
// another one can only mean the UI got its wires crossed, and dropping the quotation is better than
// dragging someone else's words in.
func (a *App) quotedIn(chat string, id int) *engine.Quote {
	if id <= 0 {
		return nil
	}
	ev, ok := a.bus.EventByID(id)
	if !ok {
		return nil
	}
	evChat, _ := ev["chat"].(string)
	if evChat != chat && !(chat == "" && evChat == "group") {
		return nil
	}
	return engine.QuoteEvent(ev)
}

func (a *App) State() map[string]any {
	bots := []map[string]any{}
	for _, w := range a.bus.Bots() {
		mcpList := w.Cfg.MCP
		if mcpList == nil {
			mcpList = []string{}
		}
		cfg := w.Cfg
		cfg.MCP = mcpList
		roots := w.Roots().List()
		if roots == nil {
			roots = []string{}
		}
		bots = append(bots, map[string]any{
			"name": w.Name(), "role": w.Cfg.RoleText(), "description": w.Cfg.DescText(),
			"provider": w.ProviderLabel(), "busy": w.Busy(), "queued": w.Queued(),
			"mcp": mcpList,

			// Directories the user named, which this member may therefore move around in freely. Part of
			// state rather than an endpoint of its own: it belongs next to the permission tier, which
			// promises "no approvals inside the workspace" without this list being able to say where that is.
			"roots": roots, "workspace": w.Workspace(),

			// The edit form needs the raw, unlocalized config — writing the display copy back would clobber it
			"config": cfg,

			// The tier actually in force: the global value when the bot sets none, so the UI can show "follow global (xx)"
			"permission": string(config.ResolvePerm(w.Cfg.Permission, a.settings.Perm())),
			"todos":      w.Todos(),
		})
	}
	approvals := []map[string]any{}
	for _, ap := range a.bus.PendingApprovals() {
		approvals = append(approvals, map[string]any{
			"id": ap.ID, "bot": ap.Bot, "action": ap.Action, "chat": ap.Chat, "dir": ap.Dir, "diff": ap.Diff,
			"kind": ap.Kind, "title": ap.Title, "body": ap.Body, "command": ap.Command,
		})
	}
	routines := []map[string]any{}
	for _, r := range a.sched.List() {
		routines = append(routines, map[string]any{
			"name": r.Name, "bot": r.Bot, "prompt": r.Prompt,
			"every_minutes": r.EveryMinutes, "next_run": r.NextRun,
		})
	}
	events := a.bus.Recent(400)
	if events == nil {
		events = []engine.Event{}
	}
	conversations := a.bus.ConversationPreviews()
	if conversations == nil {
		conversations = []engine.ConversationPreview{}
	}
	members := a.bus.GroupMembers()
	if members == nil {
		members = []string{}
	}
	tasks := a.deps.Board.List()
	return map[string]any{
		"bots": bots, "default_bot": a.bus.DefaultGroupMember(),
		"group_members": members, "groups": a.bus.Groups(), "pins": a.settings.Pins(),
		"tasks": tasks, "keys": a.deps.KS.List(),
		"mcp":       a.deps.MCP.Status(),
		"skills":    a.deps.Skills.List(),
		"plugins":   a.deps.Bundles.List(),
		"telegram":  a.tg.Status(),
		"settings":  a.settingsStatus(),
		"xai":       a.deps.XAI.Status(),
		"chatgpt":   a.deps.ChatGPT.Status(),
		"approvals": approvals, "routines": routines, "events": events, "conversations": conversations,
	}
}

// ---- HTTP ----

// cors lets the Electron renderer process (file:// origin) make cross-origin requests to the local backend.
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		// Authorization has to be allowed: the pairing code travels in that header. Without it any
		// cross-origin client is stopped by its own browser at the preflight — Electron's file:// pages
		// skip preflight, which is why this stayed hidden, but browser and remote clients simply fail.
		rw.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		next(rw, r)
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	a.registerCoreRoutes(mux)
	a.registerChatRoutes(mux)
	a.registerBotRoutes(mux)
	a.registerOAuthRoutes(mux)
	a.registerCatalogRoutes(mux)
	a.registerSettingsRoutes(mux)
	a.registerGroupRoutes(mux)
	a.registerPluginRoutes(mux)

	return mux
}

// handleSSE pushes the event stream; ?after=<id> starts right after the given event.
func (a *App) handleSSE(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		httpx.WriteJSON(rw, 500, map[string]any{"error": "streaming unsupported"})
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	rw.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(200)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		events := a.bus.EventsSinceCtx(r.Context().Done(), after, 20*time.Second)
		if len(events) == 0 {
			if _, err := fmt.Fprint(rw, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		for _, ev := range events {
			after = ev["id"].(int)
			data, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(rw, "data: %s\n\n", data); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}
