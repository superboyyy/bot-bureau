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
	"jira":       "atlassian",
	"confluence": "atlassian",
	"gh":         "github",
	"filesystem": "fs",
}

// Catalog returns the built-in connectors, ordered by setup cost (nothing to configure first).
// Desc / Need text stay in English source form so the renderer can translate via t(), and so
// list_connectors can i18n.T them for the model.
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
		{
			Name: "atlassian", Label: "Atlassian",
			Desc:  "Jira issues and Confluence pages — search, summarize, create, and update.",
			URL:   "https://mcp.atlassian.com/v1/mcp/authv2",
			OAuth: true,
		},
		{
			Name: "linear", Label: "Linear",
			Desc:  "Issues, projects, and cycles in Linear.",
			URL:   "https://mcp.linear.app/mcp",
			OAuth: true,
		},
		{
			Name: "notion", Label: "Notion",
			Desc:  "Search, read and update pages and databases in Notion.",
			URL:   "https://mcp.notion.com/mcp",
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
