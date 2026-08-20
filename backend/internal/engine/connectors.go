package engine

// Built-in connector tools: a bot can list the catalog and, with approval, install + subscribe
// itself to one. Catalog-only — the model cannot invent a command or remote URL.

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/plugin"
	"fmt"
	"strings"
)

func (t *Toolbox) runListConnectors() (string, bool) {
	var b strings.Builder
	b.WriteString(i18n.T("Built-in connectors (enable with enable_connector; requires user approval):\n"))
	for _, e := range plugin.Catalog() {
		installed := t.mcp.Has(e.Name)
		subscribed := false
		for _, s := range t.mcpServers {
			if s == e.Name {
				subscribed = true
				break
			}
		}
		status := i18n.T("not installed")
		switch {
		case subscribed:
			status = i18n.T("enabled for you")
		case installed:
			status = i18n.T("installed (not yet enabled for you)")
		}
		need := ""
		switch {
		case e.OAuth:
			need = i18n.T("; browser authorization")
		case e.Need != nil && e.Need.Kind == "key":
			have := t.ks != nil && t.ks.Get(e.Need.Key) != ""
			if have {
				need = i18n.T("; API key already saved")
			} else {
				need = i18n.T("; needs API key (pass api_key=…)")
			}
		case e.Need != nil && e.Need.Kind == "path":
			need = i18n.T("; needs path=… (directory)")
		}
		fmt.Fprintf(&b, "- %s (%s): %s [%s%s]\n", e.Name, e.Label, i18n.T(e.Desc), status, need)
	}
	b.WriteString(i18n.T("Aliases: jira/confluence→atlassian, gh→github, filesystem→fs."))
	return b.String(), false
}

func (t *Toolbox) runEnableConnector(name, path, apiKey string) (string, bool) {
	entry, ok := plugin.LookupCatalog(name)
	if !ok {
		return fmt.Sprintf(i18n.T("Unknown connector %q. Call list_connectors for the built-in list."), name), true
	}

	action := fmt.Sprintf(i18n.T("Enable connector %s (%s) for %s"), entry.Name, entry.Label, t.botName)
	act := config.ToolAct{Kind: config.ActPlugin, ReadOnly: false}
	if reason, rejected, _ := t.gate(act, action,
		i18n.T("Connector install requested, waiting for approval #%d: ")+action); rejected {
		return denied(i18n.T("The user rejected enabling this connector"), reason), true
	}

	for _, s := range t.mcpServers {
		if s == entry.Name {
			return fmt.Sprintf(i18n.T("Connector %s is already enabled for you. Use its mcp_%s_* tools."), entry.Name, entry.Name), false
		}
	}

	if entry.Need != nil && entry.Need.Kind == "key" {
		if t.ks == nil {
			return i18n.T("Key store is unavailable"), true
		}
		if t.ks.Get(entry.Need.Key) == "" {
			apiKey = strings.TrimSpace(apiKey)
			if apiKey == "" {
				return fmt.Sprintf(i18n.T("Connector %s needs an API key. Ask the user for it, then call enable_connector again with api_key set. Hint: %s"),
					entry.Label, entry.Need.Hint), true
			}
			if err := t.ks.Set(entry.Need.Key, apiKey); err != nil {
				return i18n.T("Could not save the API key: ") + err.Error(), true
			}
		}
	}

	needsAuth := false
	if !t.mcp.Has(entry.Name) {
		cfg, err := entry.ToConfig(path)
		if err != nil {
			return err.Error(), true
		}
		if err := t.mcp.Add(cfg); err != nil {
			if entry.OAuth && t.mcp.Has(entry.Name) {
				needsAuth = true
			} else if !t.mcp.Has(entry.Name) {
				return i18n.T("Could not install connector: ") + err.Error(), true
			}
		}
	}
	if entry.OAuth && t.deps != nil && t.deps.MCPAuth != nil && !t.deps.MCPAuth.Connected(entry.Name) {
		needsAuth = true
	}

	if t.deps == nil || t.deps.SubscribeMCP == nil {
		return i18n.T("Connector was installed for the team, but this bot could not subscribe (internal error)"), true
	}
	if err := t.deps.SubscribeMCP(t.botName, entry.Name); err != nil {
		return i18n.T("Installed, but failed to enable for this bot: ") + err.Error(), true
	}

	msg := fmt.Sprintf(i18n.T("Connector %s enabled. Its tools are named mcp_%s_*; they are available on your next step."), entry.Name, entry.Name)
	if needsAuth {
		t.bus.Emit("system", t.eventChat(), t.botName,
			fmt.Sprintf(i18n.T("Authorize %s in the browser to finish connecting."), entry.Label),
			map[string]any{"mcp_oauth": entry.Name})
		msg += " " + i18n.T("A browser window should open for authorization; if not, open Plugins and click Authorize.")
	}
	t.bus.Emit("refresh", "", "system",
		fmt.Sprintf(i18n.T("Plugin %s enabled for %s"), entry.Name, t.botName), nil)
	return msg, false
}
