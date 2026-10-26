package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var embeddedIndexHTML []byte

type Settings struct {
	Title                    string  `json:"title"`
	Theme                    string  `json:"theme"`
	BackgroundImage          string  `json:"background_image"`
	BackgroundOverlayOpacity float64 `json:"background_overlay_opacity"`
}

type EnvConfig struct {
	CaddyMode           string
	CaddyFilePath       string
	CaddyAPIURL         string
	DefaultLanIP        string
	SyncIntervalSeconds int
}

type Category struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Order int    `json:"order"`
}

type Service struct {
	Domain         string   `json:"domain"`
	CustomName     string   `json:"custom_name"`
	Icon           string   `json:"icon"`
	CategoryID     string   `json:"category_id"`
	Order          int      `json:"order"`
	Hidden         bool     `json:"hidden"`
	OverrideLanURL *string  `json:"override_lan_url"`
	ExternalURL    string   `json:"external_url"`
	Upstreams      []string `json:"upstreams"`
	Active         bool     `json:"active"`
	LastSeen       string   `json:"last_seen"`
}

type AppState struct {
	Settings   Settings            `json:"settings"`
	Categories []Category          `json:"categories"`
	Services   map[string]*Service `json:"services"`
}

type DiscoveredService struct {
	Domain        string
	Upstreams     []string
	MagicName     string
	MagicCategory string
	MagicIcon     string
}

type Server struct {
	mu              sync.RWMutex
	reconcileMu     sync.Mutex
	configPath      string
	uploadsDir      string
	envConfig       EnvConfig
	state           AppState
	lastFileModTime time.Time
	lastFileSize    int64
	httpClient      *http.Client
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func loadEnvConfig() EnvConfig {
	syncSec := 60
	if raw := strings.TrimSpace(os.Getenv("SYNC_INTERVAL_SECONDS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 5 {
			syncSec = parsed
		}
	}
	mode := strings.ToLower(envOrDefault("CADDY_MODE", "api"))
	if mode != "file" && mode != "api" {
		mode = "api"
	}
	return EnvConfig{
		CaddyMode:           mode,
		CaddyFilePath:       envOrDefault("CADDYFILE_PATH", "/etc/caddy/Caddyfile"),
		CaddyAPIURL:         envOrDefault("CADDY_API_URL", "http://localhost:2019/config/"),
		DefaultLanIP:        envOrDefault("DEFAULT_LAN_IP", "127.0.0.1"),
		SyncIntervalSeconds: syncSec,
	}
}

func defaultSettings() Settings {
	return Settings{
		Title:                    "CaddyHome",
		Theme:                    "dark",
		BackgroundImage:          "",
		BackgroundOverlayOpacity: 0.75,
	}
}

func defaultState() AppState {
	return AppState{
		Settings: defaultSettings(),
		Categories: []Category{
			{ID: "cat_main", Name: "Main Services", Order: 0},
			{ID: "cat_uncat", Name: "Uncategorized", Order: 99},
		},
		Services: make(map[string]*Service),
	}
}

func ensureStateDefaults(st *AppState) {
	defaults := defaultSettings()
	if strings.TrimSpace(st.Settings.Title) == "" {
		st.Settings.Title = defaults.Title
	}
	if strings.TrimSpace(st.Settings.Theme) == "" {
		st.Settings.Theme = defaults.Theme
	}
	if st.Settings.BackgroundOverlayOpacity <= 0 || st.Settings.BackgroundOverlayOpacity > 1 {
		st.Settings.BackgroundOverlayOpacity = 0.75
	}
	if st.Services == nil {
		st.Services = make(map[string]*Service)
	}
	hasUncat := false
	for _, c := range st.Categories {
		if c.ID == "cat_uncat" {
			hasUncat = true
			break
		}
	}
	if len(st.Categories) == 0 {
		st.Categories = []Category{
			{ID: "cat_main", Name: "Main Services", Order: 0},
			{ID: "cat_uncat", Name: "Uncategorized", Order: 99},
		}
	} else if !hasUncat {
		st.Categories = append(st.Categories, Category{
			ID:    "cat_uncat",
			Name:  "Uncategorized",
			Order: 99,
		})
	}
	for domain, svc := range st.Services {
		if svc == nil {
			delete(st.Services, domain)
			continue
		}
		if svc.Domain == "" {
			svc.Domain = domain
		}
		if svc.ExternalURL == "" {
			svc.ExternalURL = "https://" + svc.Domain
		}
		if svc.Upstreams == nil {
			svc.Upstreams = []string{}
		}
		if svc.CategoryID == "" {
			svc.CategoryID = "cat_uncat"
		}
		if svc.OverrideLanURL != nil && strings.TrimSpace(*svc.OverrideLanURL) == "" {
			svc.OverrideLanURL = nil
		}
	}
}

func NewServer(configPath string) (*Server, error) {
	uploadsDir := filepath.Join(filepath.Dir(configPath), "uploads")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create uploads dir: %w", err)
	}
	srv := &Server{
		configPath: configPath,
		uploadsDir: uploadsDir,
		envConfig:  loadEnvConfig(),
		state:      defaultState(),
		httpClient: &http.Client{Timeout: 3 * time.Second},
	}
	if err := srv.loadState(); err != nil {
		log.Printf("WARN: could not load existing config (%v), initializing defaults", err)
		srv.mu.Lock()
		srv.state = defaultState()
		_ = srv.saveStateLocked()
		srv.mu.Unlock()
	}
	return srv, nil
}

func (s *Server) loadState() error {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.mu.Lock()
			s.state = defaultState()
			err = s.saveStateLocked()
			s.mu.Unlock()
			return err
		}
		return err
	}
	var loaded AppState
	if err := json.Unmarshal(data, &loaded); err != nil {
		return err
	}
	ensureStateDefaults(&loaded)
	s.mu.Lock()
	s.state = loaded
	s.mu.Unlock()
	return nil
}

// saveStateLocked writes state atomically to configPath. Caller must hold s.mu.
func (s *Server) saveStateLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.configPath), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := s.configPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, s.configPath)
}

// normalizeUpstream normalizes a Caddy upstream target according to Section 5.2.
func normalizeUpstream(raw, defaultLanIP string, hasTLS bool) string {
	target := strings.TrimSpace(raw)
	if target == "" || strings.HasPrefix(target, "unix/") {
		return ""
	}
	forcedHTTPS := false
	if strings.HasPrefix(strings.ToLower(target), "https://") {
		forcedHTTPS = true
		target = target[8:]
	} else if strings.HasPrefix(strings.ToLower(target), "http://") {
		target = target[7:]
	}
	if slashIdx := strings.IndexByte(target, '/'); slashIdx != -1 {
		target = target[:slashIdx]
	}

	host := target
	port := ""
	if strings.HasPrefix(target, "[") {
		// IPv6 literal
		if closeIdx := strings.IndexByte(target, ']'); closeIdx != -1 {
			host = target[:closeIdx+1]
			rest := target[closeIdx+1:]
			if strings.HasPrefix(rest, ":") {
				port = rest[1:]
			}
		}
	} else if colonIdx := strings.LastIndexByte(target, ':'); colonIdx != -1 {
		host = target[:colonIdx]
		port = target[colonIdx+1:]
	}

	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		host = defaultLanIP
	} else if !strings.Contains(host, ".") && !strings.HasPrefix(host, "[") && !strings.EqualFold(host, "localhost") {
		host = defaultLanIP
	}

	scheme := "http"
	if forcedHTTPS || hasTLS {
		scheme = "https"
	} else {
		switch port {
		case "443", "8006", "8443", "9443":
			scheme = "https"
		}
	}

	if port != "" {
		return fmt.Sprintf("%s://%s:%s", scheme, host, port)
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func isValidConcreteDomain(d string) bool {
	d = strings.TrimSpace(d)
	if d == "" || strings.HasPrefix(d, "*.") || d == "*" || strings.HasPrefix(d, ":") || strings.HasPrefix(d, "http://") && !strings.Contains(d[7:], ".") {
		return false
	}
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	if colonIdx := strings.IndexByte(d, ':'); colonIdx != -1 {
		d = d[:colonIdx]
	}
	return strings.Contains(d, ".") && !strings.ContainsAny(d, " \t{}")
}

func cleanDomainToken(token string) string {
	token = strings.TrimSpace(strings.TrimSuffix(token, ","))
	token = strings.TrimPrefix(strings.TrimPrefix(token, "https://"), "http://")
	if slashIdx := strings.IndexByte(token, '/'); slashIdx != -1 {
		token = token[:slashIdx]
	}
	if colonIdx := strings.IndexByte(token, ':'); colonIdx != -1 {
		token = token[:colonIdx]
	}
	return strings.ToLower(strings.TrimSpace(token))
}

func resolveMagicIcon(raw string) string {
	val := strings.TrimSpace(raw)
	if val == "" {
		return ""
	}
	if strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") || strings.HasPrefix(val, "/uploads/") || strings.HasPrefix(val, "data:") {
		return val
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(val, "/"), ".png")
	return fmt.Sprintf("https://cdn.jsdelivr.net/gh/walkxcode/dashboard-icons/png/%s.png", slug)
}

func addUniqueUpstream(slice []string, item string) []string {
	if item == "" || slices.Contains(slice, item) {
		return slice
	}
	return append(slice, item)
}

// parseCaddyAPI queries the Caddy Admin API and extracts domain -> upstreams mappings.
func (s *Server) parseCaddyAPI(apiURL, defaultLanIP string) (map[string]*DiscoveredService, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if !strings.HasSuffix(endpoint, "/apps/http/servers") {
		if strings.HasSuffix(endpoint, "/config") {
			endpoint += "/apps/http/servers"
		} else {
			endpoint += "/config/apps/http/servers"
		}
	}

	resp, err := s.httpClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("caddy API returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}

	var servers map[string]any
	if err := json.Unmarshal(body, &servers); err != nil {
		return nil, err
	}

	results := make(map[string]*DiscoveredService)

	var walkRoutes func(routes []any, inheritedHosts []string)
	var walkHandlers func(handlers []any, activeHosts []string)

	walkHandlers = func(handlers []any, activeHosts []string) {
		for _, hRaw := range handlers {
			hMap, ok := hRaw.(map[string]any)
			if !ok {
				continue
			}
			handlerType, _ := hMap["handler"].(string)
			switch handlerType {
			case "subroute":
				if subRoutes, ok := hMap["routes"].([]any); ok {
					walkRoutes(subRoutes, activeHosts)
				}
			case "reverse_proxy":
				hasTLS := false
				if transport, ok := hMap["transport"].(map[string]any); ok {
					if _, tlsPresent := transport["tls"]; tlsPresent {
						hasTLS = true
					}
				}
				var extracted []string
				if upstreams, ok := hMap["upstreams"].([]any); ok {
					for _, uRaw := range upstreams {
						if uMap, ok := uRaw.(map[string]any); ok {
							if dial, ok := uMap["dial"].(string); ok {
								if norm := normalizeUpstream(dial, defaultLanIP, hasTLS); norm != "" {
									extracted = addUniqueUpstream(extracted, norm)
								}
							}
						}
					}
				}
				if len(extracted) > 0 {
					for _, host := range activeHosts {
						clean := cleanDomainToken(host)
						if !isValidConcreteDomain(clean) {
							continue
						}
						entry, exists := results[clean]
						if !exists {
							entry = &DiscoveredService{Domain: clean}
							results[clean] = entry
						}
						for _, u := range extracted {
							entry.Upstreams = addUniqueUpstream(entry.Upstreams, u)
						}
					}
				}
			default:
				if subRoutes, ok := hMap["routes"].([]any); ok {
					walkRoutes(subRoutes, activeHosts)
				}
				if subHandle, ok := hMap["handle"].([]any); ok {
					walkHandlers(subHandle, activeHosts)
				}
			}
		}
	}

	walkRoutes = func(routes []any, inheritedHosts []string) {
		for _, rRaw := range routes {
			rMap, ok := rRaw.(map[string]any)
			if !ok {
				continue
			}
			currentHosts := slices.Clone(inheritedHosts)
			if matchList, ok := rMap["match"].([]any); ok {
				var matchedHosts []string
				for _, mRaw := range matchList {
					if mMap, ok := mRaw.(map[string]any); ok {
						if hostsRaw, ok := mMap["host"].([]any); ok {
							for _, h := range hostsRaw {
								if hStr, ok := h.(string); ok && strings.TrimSpace(hStr) != "" {
									matchedHosts = append(matchedHosts, hStr)
								}
							}
						}
					}
				}
				if len(matchedHosts) > 0 {
					// Keep concrete hosts if matchedHosts has concrete ones, or refine inherited wildcard
					var concreteMatched []string
					for _, mh := range matchedHosts {
						if isValidConcreteDomain(cleanDomainToken(mh)) {
							concreteMatched = append(concreteMatched, mh)
						}
					}
					if len(concreteMatched) > 0 {
						currentHosts = concreteMatched
					} else if len(currentHosts) == 0 {
						currentHosts = matchedHosts
					}
				}
			}
			if handleList, ok := rMap["handle"].([]any); ok {
				walkHandlers(handleList, currentHosts)
			}
		}
	}

	for _, srvRaw := range servers {
		if srvMap, ok := srvRaw.(map[string]any); ok {
			if routes, ok := srvMap["routes"].([]any); ok {
				walkRoutes(routes, nil)
			}
		}
	}

	return results, nil
}

type magicMeta struct {
	name     string
	category string
	icon     string
}

var (
	reMagicName     = regexp.MustCompile(`(?i)^#\s*@name\s+(.+)$`)
	reMagicCategory = regexp.MustCompile(`(?i)^#\s*@category\s+(.+)$`)
	reMagicIcon     = regexp.MustCompile(`(?i)^#\s*@icon\s+(.+)$`)
)

// parseCaddyfile parses standard domain blocks and wildcard+named-matcher blocks with brace depth tracking.
func parseCaddyfile(filePath, defaultLanIP string) (map[string]*DiscoveredService, error) {
	rawBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return parseCaddyfileContent(string(rawBytes), defaultLanIP), nil
}

func countBracesOutsideQuotes(line string) (opens int, closes int) {
	inQuotes := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '"' && (i == 0 || line[i-1] != '\\') {
			inQuotes = !inQuotes
			continue
		}
		if !inQuotes {
			switch ch {
			case '{':
				opens++
			case '}':
				closes++
			}
		}
	}
	return opens, closes
}

func stripInlineComment(line string) string {
	inQuotes := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '"' && (i == 0 || line[i-1] != '\\') {
			inQuotes = !inQuotes
			continue
		}
		if !inQuotes && ch == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}

func extractReverseProxyTargets(tokens []string) []string {
	var targets []string
	skipFlagsWithArg := map[string]bool{
		"lb_policy":       true,
		"lb_try_duration": true,
		"lb_try_interval": true,
		"health_uri":      true,
		"health_port":     true,
		"health_interval": true,
		"health_timeout":  true,
		"health_status":   true,
		"header_up":       true,
		"header_down":     true,
		"trusted_proxies": true,
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "{" || tok == "}" {
			break
		}
		if strings.HasPrefix(tok, "@") {
			continue
		}
		if skipFlagsWithArg[tok] {
			i++
			continue
		}
		if strings.HasPrefix(tok, "-") || tok == "to" || tok == "flush_interval" {
			continue
		}
		// Likely an upstream dial address
		if strings.Contains(tok, ":") || strings.Contains(tok, ".") || strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
			targets = append(targets, tok)
		} else if len(targets) == 0 && !strings.ContainsAny(tok, "{}") {
			targets = append(targets, tok)
		}
	}
	return targets
}

func parseCaddyfileContent(content, defaultLanIP string) map[string]*DiscoveredService {
	results := make(map[string]*DiscoveredService)
	lines := strings.Split(content, "\n")

	var pendingMagic magicMeta
	depth := 0

	// Top-level block state (depth >= 1)
	var topDomains []string
	var topMagic magicMeta
	var topRawUpstreams []string
	var topHasTLS bool

	// Wildcard block matchers: @matcherName -> domains + magic
	matcherDomains := make(map[string][]string)
	matcherMagic := make(map[string]magicMeta)

	// Active handle @matcher state inside a block (depth >= 2)
	activeHandleMatcher := ""
	handleStartDepth := 0
	var handleRawUpstreams []string
	var handleHasTLS bool
	var handleInlineMagic magicMeta

	recordDomains := func(domains []string, rawUpstreams []string, hasTLS bool, meta magicMeta) {
		if len(domains) == 0 || len(rawUpstreams) == 0 {
			return
		}
		var normList []string
		for _, raw := range rawUpstreams {
			if norm := normalizeUpstream(raw, defaultLanIP, hasTLS); norm != "" {
				normList = addUniqueUpstream(normList, norm)
			}
		}
		if len(normList) == 0 {
			return
		}
		for _, d := range domains {
			clean := cleanDomainToken(d)
			if !isValidConcreteDomain(clean) {
				continue
			}
			entry, exists := results[clean]
			if !exists {
				entry = &DiscoveredService{Domain: clean}
				results[clean] = entry
			}
			for _, u := range normList {
				entry.Upstreams = addUniqueUpstream(entry.Upstreams, u)
			}
			if meta.name != "" && entry.MagicName == "" {
				entry.MagicName = meta.name
			}
			if meta.category != "" && entry.MagicCategory == "" {
				entry.MagicCategory = meta.category
			}
			if meta.icon != "" && entry.MagicIcon == "" {
				entry.MagicIcon = resolveMagicIcon(meta.icon)
			}
		}
	}

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}

		// Check magic comments before stripping comments
		if strings.HasPrefix(trimmed, "#") {
			if m := reMagicName.FindStringSubmatch(trimmed); len(m) == 2 {
				pendingMagic.name = strings.TrimSpace(m[1])
			} else if m := reMagicCategory.FindStringSubmatch(trimmed); len(m) == 2 {
				pendingMagic.category = strings.TrimSpace(m[1])
			} else if m := reMagicIcon.FindStringSubmatch(trimmed); len(m) == 2 {
				pendingMagic.icon = strings.TrimSpace(m[1])
			}
			continue
		}

		line := stripInlineComment(trimmed)
		if line == "" {
			continue
		}

		opens, closes := countBracesOutsideQuotes(line)
		prevDepth := depth

		// Depth 0: Starting a site block (or single-line site block)
		if prevDepth == 0 && opens > 0 {
			braceIdx := strings.IndexByte(line, '{')
			header := strings.TrimSpace(line[:braceIdx])
			rest := strings.TrimSpace(line[braceIdx+1:])

			// Ignore Caddy global options block `{ ... }` or snippet `(name) { ... }`
			if header != "" && !strings.HasPrefix(header, "(") {
				rawHosts := strings.FieldsFunc(header, func(r rune) bool {
					return r == ',' || r == ' ' || r == '\t'
				})
				topDomains = nil
				for _, h := range rawHosts {
					h = strings.TrimSpace(h)
					if h != "" {
						topDomains = append(topDomains, h)
					}
				}
				topMagic = pendingMagic
				pendingMagic = magicMeta{}
				topRawUpstreams = nil
				topHasTLS = false
				matcherDomains = make(map[string][]string)
				matcherMagic = make(map[string]magicMeta)
				activeHandleMatcher = ""

				// Single-line directive inside `{ ... }`
				if closeIdx := strings.LastIndexByte(rest, '}'); closeIdx != -1 {
					inner := strings.TrimSpace(rest[:closeIdx])
					if strings.Contains(inner, "tls") && strings.Contains(inner, "transport") {
						topHasTLS = true
					}
					if rpIdx := strings.Index(inner, "reverse_proxy"); rpIdx != -1 {
						rpTokens := strings.Fields(inner[rpIdx+len("reverse_proxy"):])
						topRawUpstreams = append(topRawUpstreams, extractReverseProxyTargets(rpTokens)...)
					}
				} else if rpIdx := strings.Index(rest, "reverse_proxy"); rpIdx != -1 {
					rpTokens := strings.Fields(rest[rpIdx+len("reverse_proxy"):])
					topRawUpstreams = append(topRawUpstreams, extractReverseProxyTargets(rpTokens)...)
				}
			} else {
				topDomains = nil
				pendingMagic = magicMeta{}
			}
		} else if prevDepth >= 1 && len(topDomains) > 0 {
			tokens := strings.Fields(line)
			if len(tokens) > 0 {
				// 1. Named host matcher: `@sonarr host sonarr.domain.tld`
				if strings.HasPrefix(tokens[0], "@") && len(tokens) >= 3 && tokens[1] == "host" {
					mName := tokens[0]
					var hosts []string
					for _, h := range tokens[2:] {
						if h != "{" && h != "}" {
							hosts = append(hosts, strings.TrimSuffix(h, ","))
						}
					}
					matcherDomains[mName] = hosts
					if pendingMagic.name != "" || pendingMagic.category != "" || pendingMagic.icon != "" {
						matcherMagic[mName] = pendingMagic
						pendingMagic = magicMeta{}
					}
				}

				// 2. Entering `handle @matcher {` or `route @matcher {`
				if (tokens[0] == "handle" || tokens[0] == "route") && len(tokens) >= 2 && strings.HasPrefix(tokens[1], "@") && opens > 0 {
					activeHandleMatcher = tokens[1]
					handleStartDepth = prevDepth
					handleRawUpstreams = nil
					handleHasTLS = false
					handleInlineMagic = pendingMagic
					pendingMagic = magicMeta{}

					// Check if single-line `handle @matcher { reverse_proxy ... }`
					if rpIdx := strings.Index(line, "reverse_proxy"); rpIdx != -1 {
						rpTokens := strings.Fields(line[rpIdx+len("reverse_proxy"):])
						handleRawUpstreams = append(handleRawUpstreams, extractReverseProxyTargets(rpTokens)...)
					}
				} else if tokens[0] == "reverse_proxy" {
					targets := extractReverseProxyTargets(tokens[1:])
					if activeHandleMatcher != "" {
						handleRawUpstreams = append(handleRawUpstreams, targets...)
					} else if len(tokens) >= 2 && strings.HasPrefix(tokens[1], "@") {
						// `reverse_proxy @matcher 192.168.1.30:8989`
						mName := tokens[1]
						meta := matcherMagic[mName]
						recordDomains(matcherDomains[mName], targets, strings.Contains(line, "tls"), meta)
					} else {
						topRawUpstreams = append(topRawUpstreams, targets...)
					}
				}

				// 3. Detect `tls` or `tls_*` inside `transport http { tls ... }`
				if strings.HasPrefix(tokens[0], "tls") || (tokens[0] == "transport" && strings.Contains(line, "tls")) {
					if prevDepth >= 2 || strings.Contains(line, "transport") {
						if activeHandleMatcher != "" {
							handleHasTLS = true
						} else {
							topHasTLS = true
						}
					}
				}
			}
		}

		depth += opens - closes
		if depth < 0 {
			depth = 0
		}

		// Exiting active `handle @matcher` block
		if activeHandleMatcher != "" && depth <= handleStartDepth {
			mDomains := matcherDomains[activeHandleMatcher]
			meta := matcherMagic[activeHandleMatcher]
			if meta.name == "" {
				meta.name = handleInlineMagic.name
			}
			if meta.category == "" {
				meta.category = handleInlineMagic.category
			}
			if meta.icon == "" {
				meta.icon = handleInlineMagic.icon
			}
			recordDomains(mDomains, handleRawUpstreams, handleHasTLS, meta)
			activeHandleMatcher = ""
			handleRawUpstreams = nil
			handleHasTLS = false
		}

		// Exiting top-level site block (multi-line or single-line)
		if (prevDepth > 0 || opens > 0) && depth == 0 && len(topDomains) > 0 {
			recordDomains(topDomains, topRawUpstreams, topHasTLS, topMagic)
			topDomains = nil
			topRawUpstreams = nil
			topHasTLS = false
			pendingMagic = magicMeta{}
		}
	}

	return results
}

func formatSubdomainTitle(domain string) string {
	sub := strings.ToLower(strings.Split(domain, ".")[0])
	parts := strings.FieldsFunc(sub, func(r rune) bool {
		return r == '-' || r == '_'
	})
	if len(parts) == 0 {
		return domain
	}
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func slugifyCategoryID(name string) string {
	var b strings.Builder
	b.WriteString("cat_")
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			b.WriteRune('_')
		}
	}
	res := strings.Trim(b.String(), "_")
	if res == "cat" || res == "" {
		return "cat_custom"
	}
	return res
}

// reconcileState runs discovery and updates in-memory state, writing to disk ONLY if state changed.
func (s *Server) reconcileState(force bool) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	cfg := s.envConfig
	mode := cfg.CaddyMode

	// Optimization: when watching Caddyfile in background (!force), check file mtime/size first
	if mode == "file" && !force {
		info, err := os.Stat(cfg.CaddyFilePath)
		if err != nil {
			return
		}
		if info.ModTime().Equal(s.lastFileModTime) && info.Size() == s.lastFileSize {
			return
		}
	}

	var discovered map[string]*DiscoveredService
	sourcesSucceeded := 0

	if mode == "file" {
		if info, err := os.Stat(cfg.CaddyFilePath); err == nil {
			if fileDiscovered, err := parseCaddyfile(cfg.CaddyFilePath, cfg.DefaultLanIP); err == nil {
				sourcesSucceeded++
				s.lastFileModTime = info.ModTime()
				s.lastFileSize = info.Size()
				discovered = fileDiscovered
			} else {
				log.Printf("WARN: Caddyfile parse failed: %v", err)
			}
		} else if force {
			log.Printf("WARN: Caddyfile stat failed (%s): %v", cfg.CaddyFilePath, err)
		}
	} else {
		if apiDiscovered, err := s.parseCaddyAPI(cfg.CaddyAPIURL, cfg.DefaultLanIP); err == nil {
			sourcesSucceeded++
			discovered = apiDiscovered
		} else if force {
			log.Printf("WARN: Caddy Admin API query failed: %v", err)
		}
	}

	// Do not wipe active status if discovery source failed (e.g. Caddy restarting)
	if sourcesSucceeded == 0 {
		return
	}

	nowUTC := time.Now().UTC().Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()

	dirty := false

	findOrCreateCategory := func(catName string) string {
		catName = strings.TrimSpace(catName)
		if catName == "" {
			return "cat_uncat"
		}
		for _, c := range s.state.Categories {
			if strings.EqualFold(c.Name, catName) {
				return c.ID
			}
		}
		newID := slugifyCategoryID(catName)
		for _, c := range s.state.Categories {
			if c.ID == newID {
				return c.ID
			}
		}
		maxOrder := 0
		for _, c := range s.state.Categories {
			if c.ID != "cat_uncat" && c.Order >= maxOrder {
				maxOrder = c.Order + 1
			}
		}
		s.state.Categories = append(s.state.Categories, Category{
			ID:    newID,
			Name:  catName,
			Order: maxOrder,
		})
		dirty = true
		return newID
	}

	nextOrderInCategory := func(catID string) int {
		next := 0
		for _, svc := range s.state.Services {
			if svc.CategoryID == catID && svc.Order >= next {
				next = svc.Order + 1
			}
		}
		return next
	}

	// Deterministic iteration order
	domains := make([]string, 0, len(discovered))
	for dom := range discovered {
		domains = append(domains, dom)
	}
	sort.Strings(domains)

	for _, dom := range domains {
		dSvc := discovered[dom]
		if existing, exists := s.state.Services[dom]; !exists {
			customName := dSvc.MagicName
			if customName == "" {
				customName = formatSubdomainTitle(dom)
			}
			catID := findOrCreateCategory(dSvc.MagicCategory)
			s.state.Services[dom] = &Service{
				Domain:         dom,
				CustomName:     customName,
				Icon:           dSvc.MagicIcon, // Empty "" by default so frontend uses link's own favicon
				CategoryID:     catID,
				Order:          nextOrderInCategory(catID),
				Hidden:         false,
				OverrideLanURL: nil,
				ExternalURL:    "https://" + dom,
				Upstreams:      dSvc.Upstreams,
				Active:         true,
				LastSeen:       nowUTC,
			}
			dirty = true
		} else {
			upstreamsChanged := !slices.Equal(existing.Upstreams, dSvc.Upstreams)
			activeChanged := !existing.Active
			if upstreamsChanged || activeChanged {
				existing.Upstreams = dSvc.Upstreams
				existing.Active = true
				existing.LastSeen = nowUTC
				dirty = true
			}
		}
	}

	// Mark services missing from current discovery as Active = false
	for dom, existing := range s.state.Services {
		if _, present := discovered[dom]; !present {
			if existing.Active {
				existing.Active = false
				dirty = true
			}
		}
	}

	if dirty {
		if err := s.saveStateLocked(); err != nil {
			log.Printf("ERROR: failed saving state to %s: %v", s.configPath, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(embeddedIndexHTML)
}

func (s *Server) handleGetState(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, s.state)
}

func (s *Server) handlePostSync(w http.ResponseWriter, _ *http.Request) {
	s.reconcileState(true)
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, s.state)
}

func (s *Server) handlePutState(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed reading request body"})
		return
	}
	var incoming AppState
	if err := json.Unmarshal(body, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload: " + err.Error()})
		return
	}
	ensureStateDefaults(&incoming)

	s.mu.Lock()
	s.state = incoming
	err = s.saveStateLocked()
	s.mu.Unlock()

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed persisting state: " + err.Error()})
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, s.state)
}

var allowedImageExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".svg":  true,
	".ico":  true,
	".gif":  true,
}

func sanitizeFilename(name string) string {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	res := b.String()
	if res == "" {
		return "img"
	}
	if len(res) > 32 {
		return res[:32]
	}
	return res
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10 MB max
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file too large or invalid multipart form (max 10MB)"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing 'file' field"})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !allowedImageExts[ext] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported image format"})
		return
	}

	randBytes := make([]byte, 4)
	_, _ = rand.Read(randBytes)
	filename := fmt.Sprintf("%d-%s-%s%s", time.Now().Unix(), hex.EncodeToString(randBytes), sanitizeFilename(header.Filename), ext)
	destPath := filepath.Join(s.uploadsDir, filename)

	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed creating upload file"})
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		_ = os.Remove(destPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed saving uploaded file"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"url": "/uploads/" + filename,
	})
}

func (s *Server) startBackgroundSync() {
	if s.envConfig.CaddyMode != "file" {
		return
	}
	intervalSec := s.envConfig.SyncIntervalSeconds
	if intervalSec < 5 {
		intervalSec = 60
	}
	go func() {
		ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.reconcileState(false)
		}
	}()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.Handle("GET /uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.uploadsDir))))
	mux.HandleFunc("GET /api/state", s.handleGetState)
	mux.HandleFunc("POST /api/sync", s.handlePostSync)
	mux.HandleFunc("PUT /api/state", s.handlePutState)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	return mux
}

func main() {
	configPath := envOrDefault("CONFIG_PATH", "/data/config.json")
	port := envOrDefault("PORT", "8080")

	srv, err := NewServer(configPath)
	if err != nil {
		log.Fatalf("Failed initializing CaddyHome server: %v", err)
	}

	// Initial reconciliation on startup
	srv.reconcileState(true)
	srv.startBackgroundSync()

	addr := ":" + port
	log.Printf("CaddyHome listening on %s (config: %s)", addr, configPath)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
