package plugin

// Built-in connector catalog: the one-click installs in the plugins panel, and the entries a bot
// may enable from chat via list_connectors / enable_connector. Kept here (not in the renderer) so
// both surfaces share one list and a bot cannot invent an arbitrary command or URL.

import (
	"botbureau/backend/internal/i18n"
	"fmt"
	"strings"
)

// CatalogNeed is the one piece of setup a catalog entry still needs from the user.
type CatalogNeed struct {
	Kind        string `json:"kind"`          // "key" | "path"
	Key         string `json:"key,omitempty"` // key-store name
	As          string `json:"as,omitempty"`  // "bearer" | "env"
	Label       string `json:"label,omitempty"`
	Hint        string `json:"hint,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// CatalogEntry is one installable connector.
type CatalogEntry struct {
	Name    string       `json:"name"`
	Label   string       `json:"label"`
	Desc    string       `json:"desc"`
	Command string       `json:"command,omitempty"`
	Args    string       `json:"args,omitempty"` // space-separated, matching /api/mcp/add
	URL     string       `json:"url,omitempty"`
	OAuth   bool         `json:"oauth,omitempty"`
	Slow    bool         `json:"slow,omitempty"`
	Need    *CatalogNeed `json:"need,omitempty"`
}

// catalogAliases maps everyday names (jira, gh, …) onto a catalog id.
var catalogAliases = map[string]string{
	"jira":            "atlassian",
	"confluence":      "atlassian",
	"gh":              "github",
	"filesystem":      "fs",
	"drive":           "google-drive",
	"gdrive":          "google-drive",
	"google_drive":    "google-drive",
	"calendar":        "google-calendar",
	"gcal":            "google-calendar",
	"google_calendar": "google-calendar",
	"crm":             "hubspot",
	"payments":        "stripe",
}

// Catalog returns the built-in connectors, ordered by setup cost (nothing to configure first).
// Desc / Need text stay in English source form so the renderer can translate via t(), and so
// list_connectors can i18n.T them for the model.
//
// Remote OAuth entries mirror the connectors Claude Code / Codex document most often
// (Slack, Figma, Asana, Stripe, Google Workspace, HubSpot, …).
func Catalog() []CatalogEntry {
	return []CatalogEntry{
		{
			Name: "deepwiki", Label: "DeepWiki",
			Desc: "Ask questions about any public GitHub repository — its documentation, architecture, and code.",
			URL:  "https://mcp.deepwiki.com/mcp",
		},
		{
			Name: "context7", Label: "Context7",
			Desc: "Up-to-date documentation and code examples for thousands of libraries.",
			URL:  "https://mcp.context7.com/mcp",
		},
		{
			Name: "memory", Label: "Memory",
			Desc:    "A knowledge graph where the team can record and search facts later.",
			Command: "npx", Args: "-y @modelcontextprotocol/server-memory",
		},
		{
			Name: "thinking", Label: "Sequential Thinking",
			Desc:    "A scratchpad for working through a hard problem one step at a time.",
			Command: "npx", Args: "-y @modelcontextprotocol/server-sequential-thinking",
		},
		{
			Name: "fs", Label: "Filesystem",
			Desc:    "Read and write files under one directory you choose.",
			Command: "npx", Args: "-y @modelcontextprotocol/server-filesystem",
			Need: &CatalogNeed{Kind: "path", Label: "Directory to expose", Placeholder: "/Users/you/Documents"},
		},
		{
			Name: "playwright", Label: "Playwright",
			Desc:    "Drive a real browser: open pages, click, fill forms, read what is on screen.",
			Command: "npx", Args: "-y @playwright/mcp@latest",
			Slow: true,
		},
		{
			Name: "github", Label: "GitHub",
			Desc: "Issues, pull requests, code search, and file contents on GitHub.",
			URL:  "https://api.githubcopilot.com/mcp/",
			Need: &CatalogNeed{
				Kind: "key", Key: "GITHUB_PAT", As: "bearer",
				Label: "GitHub personal access token",
				Hint:  "Create a token at github.com/settings/personal-access-tokens — it is stored on this machine only.",
			},
		},
		{
			Name: "exa", Label: "Exa Search",
			Desc:    "AI-focused web search that returns full page content, not just links.",
			Command: "npx", Args: "-y exa-mcp-server",
			Need: &CatalogNeed{
				Kind: "key", Key: "EXA_API_KEY", As: "env",
				Label: "Exa API key", Hint: "Get one from exa.ai.",
			},
		},
		{
			Name: "firecrawl", Label: "Firecrawl",
			Desc:    "Turn entire websites into clean Markdown.",
			Command: "npx", Args: "-y firecrawl-mcp",
			Need: &CatalogNeed{
				Kind: "key", Key: "FIRECRAWL_API_KEY", As: "env",
				Label: "Firecrawl API key", Hint: "Get one from firecrawl.dev.",
			},
		},

		// Remote OAuth — same hosts Claude Code / Codex list for everyday team tools.
		{
			Name: "slack", Label: "Slack",
			Desc:  "Search channels, read threads, and post messages in Slack.",
			URL:   "https://mcp.slack.com/mcp",
			OAuth: true,
		},
		{
			Name: "figma", Label: "Figma",
			Desc:  "Read Figma files, components, and design context for implementation.",
			URL:   "https://mcp.figma.com/mcp",
			OAuth: true,
		},
		{
			Name: "asana", Label: "Asana",
			Desc:  "Tasks, projects, and timelines in Asana.",
			URL:   "https://mcp.asana.com/mcp",
			OAuth: true,
		},
		{
			Name: "monday", Label: "monday.com",
			Desc:  "Boards, items, and workflows in monday.com.",
			URL:   "https://mcp.monday.com/mcp",
			OAuth: true,
		},
		{
			Name: "linear", Label: "Linear",
			Desc:  "Issues, projects, and cycles in Linear.",
			URL:   "https://mcp.linear.app/mcp",
			OAuth: true,
		},
		{
			Name: "atlassian", Label: "Atlassian",
			Desc:  "Jira issues and Confluence pages — search, summarize, create, and update.",
			URL:   "https://mcp.atlassian.com/v1/mcp/authv2",
			OAuth: true,
		},
		{
			Name: "notion", Label: "Notion",
			Desc:  "Search, read and update pages and databases in Notion.",
			URL:   "https://mcp.notion.com/mcp",
			OAuth: true,
		},
		{
			Name: "google-drive", Label: "Google Drive",
			Desc:  "Search and read files in Google Drive.",
			URL:   "https://drivemcp.googleapis.com/mcp/v1",
			OAuth: true,
		},
		{
			Name: "gmail", Label: "Gmail",
			Desc:  "Search, read, and draft email in Gmail.",
			URL:   "https://gmailmcp.googleapis.com/mcp/v1",
			OAuth: true,
		},
		{
			Name: "google-calendar", Label: "Google Calendar",
			Desc:  "List and manage events in Google Calendar.",
			URL:   "https://calendarmcp.googleapis.com/mcp/v1",
			OAuth: true,
		},
		{
			Name: "box", Label: "Box",
			Desc:  "Search and work with files in Box.",
			URL:   "https://mcp.box.com",
			OAuth: true,
		},
		{
			Name: "dropbox", Label: "Dropbox",
			Desc:  "Search and work with files in Dropbox.",
			URL:   "https://mcp.dropbox.com/mcp",
			OAuth: true,
		},
		{
			Name: "canva", Label: "Canva",
			Desc:  "Create and edit designs in Canva.",
			URL:   "https://mcp.canva.com/mcp",
			OAuth: true,
		},
		{
			Name: "stripe", Label: "Stripe",
			Desc:  "Customers, payments, invoices, and Stripe account data.",
			URL:   "https://mcp.stripe.com",
			OAuth: true,
		},
		{
			Name: "hubspot", Label: "HubSpot",
			Desc:  "CRM contacts, deals, tickets, and marketing data in HubSpot.",
			URL:   "https://mcp.hubspot.com",
			OAuth: true,
		},
		{
			Name: "intercom", Label: "Intercom",
			Desc:  "Customer conversations and support data in Intercom.",
			URL:   "https://mcp.intercom.com/mcp",
			OAuth: true,
		},
		{
			Name: "supabase", Label: "Supabase",
			Desc:  "Tables, auth, and storage in a Supabase project.",
			URL:   "https://mcp.supabase.com/mcp",
			OAuth: true,
		},
		{
			Name: "neon", Label: "Neon",
			Desc:  "Serverless Postgres databases on Neon.",
			URL:   "https://mcp.neon.tech/mcp",
			OAuth: true,
		},
		{
			Name: "vercel", Label: "Vercel",
			Desc:  "Projects, deployments, and logs on Vercel.",
			URL:   "https://mcp.vercel.com",
			OAuth: true,
		},
		{
			Name: "sentry", Label: "Sentry",
			Desc:  "Search errors, inspect issues, and dig into performance in Sentry.",
			URL:   "https://mcp.sentry.dev/mcp",
			OAuth: true,
		},
	}
}

// ResolveCatalogName maps an alias or label-ish id onto a catalog name.
func ResolveCatalogName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if alt, ok := catalogAliases[n]; ok {
		return alt
	}
	return n
}

// LookupCatalog finds one entry by name or alias.
func LookupCatalog(name string) (CatalogEntry, bool) {
	want := ResolveCatalogName(name)
	for _, e := range Catalog() {
		if e.Name == want {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// ToConfig builds the mcp.yaml shape for this entry. pathArg is required when Need.Kind == "path";
// the key itself is assumed already in the key store (or not needed).
func (e CatalogEntry) ToConfig(pathArg string) (MCPServerConfig, error) {
	cfg := MCPServerConfig{Name: e.Name}
	if e.URL != "" {
		cfg.URL = e.URL
		if e.OAuth {
			cfg.Auth = "oauth"
		}
		if e.Need != nil && e.Need.As == "bearer" {
			cfg.BearerKey = e.Need.Key
		}
		return cfg, nil
	}
	if e.Command == "" {
		return cfg, fmt.Errorf(i18n.T("Catalog entry %s has neither url nor command"), e.Name)
	}
	cfg.Command = e.Command
	cfg.Args = strings.Fields(e.Args)
	if e.Need != nil && e.Need.Kind == "path" {
		pathArg = strings.TrimSpace(pathArg)
		if pathArg == "" {
			return cfg, fmt.Errorf(i18n.T("Connector %s needs a directory path"), e.Label)
		}
		cfg.Args = append(cfg.Args, pathArg)
	}
	if e.Need != nil && e.Need.As == "env" {
		cfg.Env = map[string]string{e.Need.Key: "$" + e.Need.Key}
	}
	return cfg, nil
}
