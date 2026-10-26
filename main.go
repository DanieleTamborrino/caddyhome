package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed index.html
var embeddedIndexHTML []byte

//go:embed logo.svg
var embeddedLogoSVG []byte

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
	PUID                int
	PGID                int
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

type cachedFavicon struct {
	data        []byte
	contentType string
	notFound    bool
	expiresAt   time.Time
}

type faviconInflight struct {
	done  chan struct{}
	entry cachedFavicon
}

type Server struct {
	mu              sync.RWMutex
	reconcileMu     sync.Mutex
	faviconMu       sync.RWMutex
	faviconCache    map[string]cachedFavicon
	faviconInflight map[string]*faviconInflight
	faviconClient   *http.Client
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

func parseEnvID(key string) int {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			return parsed
		}
	}
	return -1
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
		PUID:                parseEnvID("PUID"),
		PGID:                parseEnvID("PGID"),
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

func defaultCategories() []Category {
	return []Category{
		{ID: "cat_main", Name: "Main Services", Order: 0},
		{ID: "cat_uncat", Name: "Uncategorized", Order: 99},
	}
}

func defaultState() AppState {
	return AppState{
		Settings:   defaultSettings(),
		Categories: defaultCategories(),
		Services:   make(map[string]*Service),
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
	var cleanedCats []Category
	seenCats := make(map[string]bool, len(st.Categories))
	hasUncat := false
	for _, c := range st.Categories {
		c.ID = strings.TrimSpace(c.ID)
		c.Name = strings.TrimSpace(c.Name)
		if c.ID == "" || seenCats[c.ID] {
			continue
		}
		if c.Name == "" {
			c.Name = c.ID
		}
		seenCats[c.ID] = true
		if c.ID == "cat_uncat" {
			hasUncat = true
		}
		cleanedCats = append(cleanedCats, c)
	}
	st.Categories = cleanedCats
	if len(st.Categories) == 0 {
		st.Categories = defaultCategories()
	} else if !hasUncat {
		st.Categories = append(st.Categories, Category{
			ID:    "cat_uncat",
			Name:  "Uncategorized",
			Order: 99,
		})
	}
	validCats := make(map[string]bool, len(st.Categories))
	for _, c := range st.Categories {
		validCats[c.ID] = true
	}
	cleanedServices := make(map[string]*Service, len(st.Services))
	for domain, svc := range st.Services {
		if svc == nil {
			continue
		}
		key := strings.TrimSpace(domain)
		if key == "" {
			key = strings.TrimSpace(svc.Domain)
		}
		if key == "" {
			continue
		}
		svc.Domain = key
		if svc.ExternalURL == "" {
			svc.ExternalURL = "https://" + svc.Domain
		}
		if svc.Upstreams == nil {
			svc.Upstreams = []string{}
		}
		if svc.CategoryID == "" || !validCats[svc.CategoryID] {
			svc.CategoryID = "cat_uncat"
		}
		if svc.OverrideLanURL != nil && strings.TrimSpace(*svc.OverrideLanURL) == "" {
			svc.OverrideLanURL = nil
		}
		cleanedServices[key] = svc
	}
	st.Services = cleanedServices
}

func (s *Server) applyOwnership(path string) {
	if s.envConfig.PUID >= 0 || s.envConfig.PGID >= 0 {
		_ = os.Chown(path, s.envConfig.PUID, s.envConfig.PGID)
	}
}

func NewServer(configPath string) (*Server, error) {
	dataDir := filepath.Dir(configPath)
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create uploads dir: %w", err)
	}
	favTransport := http.DefaultTransport.(*http.Transport).Clone()
	favTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	srv := &Server{
		configPath:      configPath,
		uploadsDir:      uploadsDir,
		envConfig:       loadEnvConfig(),
		state:           defaultState(),
		faviconCache:    make(map[string]cachedFavicon),
		faviconInflight: make(map[string]*faviconInflight),
		faviconClient: &http.Client{
			Timeout:   4 * time.Second,
			Transport: favTransport,
		},
		httpClient: &http.Client{Timeout: 3 * time.Second},
	}
	srv.applyOwnership(dataDir)
	srv.applyOwnership(uploadsDir)
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
	if normalized, err := json.MarshalIndent(s.state, "", "  "); err == nil && !bytes.Equal(bytes.TrimSpace(normalized), bytes.TrimSpace(data)) {
		_ = s.saveStatePayloadLocked(normalized)
	} else {
		s.applyOwnership(s.configPath)
	}
	s.mu.Unlock()
	return nil
}

// saveStateLocked writes state atomically to configPath. Caller must hold s.mu.
func (s *Server) saveStateLocked() error {
	payload, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return s.saveStatePayloadLocked(payload)
}

func (s *Server) saveStatePayloadLocked(payload []byte) error {
	dataDir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	s.applyOwnership(dataDir)
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
	s.applyOwnership(tmpPath)
	return os.Rename(tmpPath, s.configPath)
}

// normalizeUpstream normalizes a Caddy upstream target according to Section 5.2.
func normalizeUpstream(raw, defaultLanIP string, hasTLS bool) string {
	target := strings.TrimSpace(raw)
	if target == "" || strings.HasPrefix(target, "unix/") || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "*") {
		return ""
	}
	forcedHTTPS := false
	forcedHTTP := false
	lowerTarget := strings.ToLower(target)
	if strings.HasPrefix(lowerTarget, "https://") {
		forcedHTTPS = true
		target = target[8:]
	} else if strings.HasPrefix(lowerTarget, "http://") {
		forcedHTTP = true
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
	if host == "" && port == "" {
		return ""
	}
	if host == "" {
		host = defaultLanIP
	} else if !strings.Contains(host, ".") && !strings.HasPrefix(host, "[") && !strings.EqualFold(host, "localhost") {
		host = defaultLanIP
	}

	scheme := "http"
	if forcedHTTPS || hasTLS {
		scheme = "https"
	} else if !forcedHTTP {
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
	d = cleanDomainToken(d)
	if d == "" || strings.Contains(d, "*") || strings.HasPrefix(d, ":") {
		return false
	}
	return strings.Contains(d, ".") && !strings.ContainsAny(d, " \t{}")
}

func cleanDomainToken(token string) string {
	token = strings.ToLower(strings.TrimSpace(token))
	token = strings.TrimSpace(strings.TrimSuffix(token, ","))
	token = strings.TrimPrefix(strings.TrimPrefix(token, "https://"), "http://")
	if slashIdx := strings.IndexByte(token, '/'); slashIdx != -1 {
		token = token[:slashIdx]
	}
	if colonIdx := strings.IndexByte(token, ':'); colonIdx != -1 {
		token = token[:colonIdx]
	}
	return strings.TrimSpace(token)
}

func resolveMagicIcon(raw string) string {
	val := strings.TrimSpace(raw)
	if val == "" {
		return ""
	}
	lowerVal := strings.ToLower(val)
	if strings.HasPrefix(lowerVal, "http://") || strings.HasPrefix(lowerVal, "https://") || strings.HasPrefix(val, "/uploads/") || strings.HasPrefix(lowerVal, "data:") {
		return val
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(lowerVal, "/"), ".png")
	return fmt.Sprintf("https://cdn.jsdelivr.net/gh/walkxcode/dashboard-icons/png/%s.png", slug)
}

func addUniqueUpstream(slice []string, item string) []string {
	if item == "" || slices.Contains(slice, item) {
		return slice
	}
	return append(slice, item)
}

func upsertDiscoveredService(results map[string]*DiscoveredService, rawDomain string, upstreams []string) *DiscoveredService {
	clean := cleanDomainToken(rawDomain)
	if !isValidConcreteDomain(clean) {
		return nil
	}
	entry, exists := results[clean]
	if !exists {
		entry = &DiscoveredService{Domain: clean}
		results[clean] = entry
	}
	for _, u := range upstreams {
		entry.Upstreams = addUniqueUpstream(entry.Upstreams, u)
	}
	return entry
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
						upsertDiscoveredService(results, host, extracted)
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

func sanitizeLineAndCountBraces(line string) (cleanLine string, opens int, closes int) {
	inQuotes := false
	escaped := false
	commentIdx := len(line)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inQuotes {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuotes = !inQuotes
			continue
		}
		if !inQuotes {
			if ch == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				commentIdx = i
				break
			}
			switch ch {
			case '{':
				opens++
			case '}':
				closes++
			}
		}
	}
	return strings.TrimSpace(line[:commentIdx]), opens, closes
}

var skipReverseProxyFlags = map[string]int{
	"lb_policy":       1,
	"lb_try_duration": 1,
	"lb_try_interval": 1,
	"health_uri":      1,
	"health_port":     1,
	"health_interval": 1,
	"health_timeout":  1,
	"health_status":   1,
	"trusted_proxies": 1,
	"flush_interval":  1,
}

func extractReverseProxyTargets(tokens []string) []string {
	var targets []string
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "{" || tok == "}" {
			break
		}
		if strings.HasPrefix(tok, "@") || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "*") {
			continue
		}
		if skipArgs, ok := skipReverseProxyFlags[tok]; ok {
			i += skipArgs
			continue
		}
		if tok == "header_up" || tok == "header_down" {
			// Syntax: header_up [+|-]Field [value]
			if i+1 < len(tokens) {
				field := tokens[i+1]
				i++
				if !strings.HasPrefix(field, "-") && i+1 < len(tokens) && tokens[i+1] != "{" && tokens[i+1] != "}" {
					i++
				}
			}
			continue
		}
		if strings.HasPrefix(tok, "-") || tok == "to" {
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

func parseInlineReverseProxy(segment string) (targets []string, hasTLS bool, ok bool) {
	rpIdx := strings.Index(segment, "reverse_proxy")
	if rpIdx == -1 {
		return nil, false, false
	}
	rpPart := segment[rpIdx+len("reverse_proxy"):]
	return extractReverseProxyTargets(strings.Fields(rpPart)), strings.Contains(rpPart, "transport") && strings.Contains(rpPart, "tls"), true
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

	// Active multi-line @matcher { host ... } definition
	activeMatcherDef := ""
	matcherDefStartDepth := 0

	// Active handle @matcher state inside a block (depth >= 2)
	activeHandleMatcher := ""
	handleStartDepth := 0
	var handleRawUpstreams []string
	var handleHasTLS bool
	var handleInlineMagic magicMeta

	// Active multi-line reverse_proxy block state
	inReverseProxyBlock := false
	rpStartDepth := 0
	rpDirectMatcher := ""
	var rpDirectRawUpstreams []string
	var rpDirectHasTLS bool
	var rpDirectMagic magicMeta

	mergeMagic := func(base, override magicMeta) magicMeta {
		if override.name != "" {
			base.name = override.name
		}
		if override.category != "" {
			base.category = override.category
		}
		if override.icon != "" {
			base.icon = override.icon
		}
		return base
	}

	recordDomains := func(domains []string, rawUpstreams []string, hasTLS bool, meta magicMeta, matcherTag string) {
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
		derivedMatcherName := formatMatcherTitle(matcherTag)
		for _, d := range domains {
			entry := upsertDiscoveredService(results, d, normList)
			if entry == nil {
				continue
			}
			if meta.name != "" {
				entry.MagicName = meta.name
			} else if derivedMatcherName != "" && entry.MagicName == "" {
				entry.MagicName = derivedMatcherName
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

		line, opens, closes := sanitizeLineAndCountBraces(trimmed)
		if line == "" {
			continue
		}

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
				activeMatcherDef = ""
				activeHandleMatcher = ""
				inReverseProxyBlock = false
				rpDirectMatcher = ""

				// Single-line directive inside `{ ... }`
				if targets, inlineTLS, ok := parseInlineReverseProxy(rest); ok {
					topRawUpstreams = append(topRawUpstreams, targets...)
					if inlineTLS {
						topHasTLS = true
					}
				}
			} else {
				topDomains = nil
				pendingMagic = magicMeta{}
			}
		} else if prevDepth >= 1 && len(topDomains) > 0 {
			tokens := strings.Fields(line)
			if len(tokens) > 0 {
				// 1. Named host matcher: `@sonarr host sonarr.domain.tld` or `@sonarr { host sonarr.domain.tld }`
				if strings.HasPrefix(tokens[0], "@") && len(tokens) >= 2 {
					mName := tokens[0]
					if tokens[1] == "host" && len(tokens) >= 3 {
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
					} else if tokens[1] == "{" {
						if pendingMagic.name != "" || pendingMagic.category != "" || pendingMagic.icon != "" {
							matcherMagic[mName] = pendingMagic
							pendingMagic = magicMeta{}
						}
						// Check if `host ...` is on the same line inside `{ ... }`
						for idx := 2; idx < len(tokens); idx++ {
							if tokens[idx] == "host" {
								for _, h := range tokens[idx+1:] {
									if h == "}" || h == "{" {
										break
									}
									matcherDomains[mName] = append(matcherDomains[mName], strings.TrimSuffix(h, ","))
								}
								break
							}
						}
						if opens > closes {
							activeMatcherDef = mName
							matcherDefStartDepth = prevDepth
						}
					}
				} else if activeMatcherDef != "" && tokens[0] == "host" && len(tokens) >= 2 {
					for _, h := range tokens[1:] {
						if h != "{" && h != "}" {
							matcherDomains[activeMatcherDef] = append(matcherDomains[activeMatcherDef], strings.TrimSuffix(h, ","))
						}
					}
				}

				// 2. Entering `handle @matcher {` or `route @matcher {` for a known host matcher
				isHandlerDirective := tokens[0] == "handle" || tokens[0] == "handle_path" || tokens[0] == "route"
				if activeHandleMatcher == "" && isHandlerDirective && len(tokens) >= 2 && strings.HasPrefix(tokens[1], "@") && len(matcherDomains[tokens[1]]) > 0 && opens > 0 {
					activeHandleMatcher = tokens[1]
					handleStartDepth = prevDepth
					handleRawUpstreams = nil
					handleHasTLS = false
					handleInlineMagic = pendingMagic
					pendingMagic = magicMeta{}

					// Check if single-line `handle @matcher { reverse_proxy ... }`
					if targets, inlineTLS, ok := parseInlineReverseProxy(line); ok {
						handleRawUpstreams = append(handleRawUpstreams, targets...)
						if inlineTLS {
							handleHasTLS = true
						}
					}
				} else if isHandlerDirective {
					// Single-line `handle /path/* { reverse_proxy ... }` or `handle_path /api/* { reverse_proxy ... }`
					if targets, inlineTLS, ok := parseInlineReverseProxy(line); ok {
						if activeHandleMatcher != "" {
							handleRawUpstreams = append(handleRawUpstreams, targets...)
							if inlineTLS {
								handleHasTLS = true
							}
						} else {
							topRawUpstreams = append(topRawUpstreams, targets...)
							if inlineTLS {
								topHasTLS = true
							}
						}
					}
				} else if tokens[0] == "reverse_proxy" {
					targets, inlineTLS, _ := parseInlineReverseProxy(line)
					if activeHandleMatcher != "" {
						handleRawUpstreams = append(handleRawUpstreams, targets...)
						if inlineTLS {
							handleHasTLS = true
						}
						if pendingMagic.name != "" || pendingMagic.category != "" || pendingMagic.icon != "" {
							handleInlineMagic = mergeMagic(handleInlineMagic, pendingMagic)
							pendingMagic = magicMeta{}
						}
						if opens > closes {
							inReverseProxyBlock = true
							rpStartDepth = prevDepth
							rpDirectMatcher = ""
						}
					} else if len(tokens) >= 2 && strings.HasPrefix(tokens[1], "@") && len(matcherDomains[tokens[1]]) > 0 {
						// `reverse_proxy @matcher 192.168.1.30:8989` (single-line or multi-line block for a host matcher)
						mName := tokens[1]
						baseMeta := magicMeta{category: topMagic.category, icon: topMagic.icon}
						meta := mergeMagic(mergeMagic(baseMeta, matcherMagic[mName]), pendingMagic)
						pendingMagic = magicMeta{}
						if opens > closes {
							inReverseProxyBlock = true
							rpStartDepth = prevDepth
							rpDirectMatcher = mName
							rpDirectRawUpstreams = targets
							rpDirectHasTLS = inlineTLS
							rpDirectMagic = meta
						} else {
							recordDomains(matcherDomains[mName], targets, inlineTLS, meta, mName)
						}
					} else {
						topRawUpstreams = append(topRawUpstreams, targets...)
						if inlineTLS {
							topHasTLS = true
						}
						if pendingMagic.name != "" || pendingMagic.category != "" || pendingMagic.icon != "" {
							topMagic = mergeMagic(topMagic, pendingMagic)
							pendingMagic = magicMeta{}
						}
						if opens > closes {
							inReverseProxyBlock = true
							rpStartDepth = prevDepth
							rpDirectMatcher = ""
						}
					}
				} else if inReverseProxyBlock {
					// Inside `reverse_proxy ... { ... }` block
					if tokens[0] == "to" && len(tokens) >= 2 {
						targets := extractReverseProxyTargets(tokens[1:])
						if activeHandleMatcher != "" {
							handleRawUpstreams = append(handleRawUpstreams, targets...)
						} else if rpDirectMatcher != "" {
							rpDirectRawUpstreams = append(rpDirectRawUpstreams, targets...)
						} else {
							topRawUpstreams = append(topRawUpstreams, targets...)
						}
					}
					if strings.HasPrefix(tokens[0], "tls") || (tokens[0] == "transport" && strings.Contains(line, "tls")) {
						if activeHandleMatcher != "" {
							handleHasTLS = true
						} else if rpDirectMatcher != "" {
							rpDirectHasTLS = true
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

		// Exiting multi-line `@matcher { ... }` definition block
		if activeMatcherDef != "" && depth <= matcherDefStartDepth {
			activeMatcherDef = ""
		}

		// Exiting multi-line `reverse_proxy { ... }` block
		if inReverseProxyBlock && depth <= rpStartDepth {
			if rpDirectMatcher != "" {
				recordDomains(matcherDomains[rpDirectMatcher], rpDirectRawUpstreams, rpDirectHasTLS, rpDirectMagic, rpDirectMatcher)
			}
			inReverseProxyBlock = false
			rpDirectMatcher = ""
			rpDirectRawUpstreams = nil
			rpDirectHasTLS = false
			rpDirectMagic = magicMeta{}
		}

		// Exiting active `handle @matcher` block
		if activeHandleMatcher != "" && depth <= handleStartDepth {
			mDomains := matcherDomains[activeHandleMatcher]
			baseMeta := magicMeta{category: topMagic.category, icon: topMagic.icon}
			meta := mergeMagic(mergeMagic(baseMeta, matcherMagic[activeHandleMatcher]), handleInlineMagic)
			recordDomains(mDomains, handleRawUpstreams, handleHasTLS, meta, activeHandleMatcher)
			activeHandleMatcher = ""
			handleRawUpstreams = nil
			handleHasTLS = false
			handleInlineMagic = magicMeta{}
		}

		// Exiting top-level site block (multi-line or single-line)
		if (prevDepth > 0 || opens > 0) && depth == 0 && len(topDomains) > 0 {
			recordDomains(topDomains, topRawUpstreams, topHasTLS, topMagic, "")
			topDomains = nil
			topRawUpstreams = nil
			topHasTLS = false
			pendingMagic = magicMeta{}
		}
	}

	return results
}

func formatWordsTitle(raw string) string {
	parts := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(raw)), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if len(parts) == 0 {
		return ""
	}
	for i, p := range parts {
		if r, size := utf8.DecodeRuneInString(p); r != utf8.RuneError && size > 0 {
			parts[i] = string(unicode.ToUpper(r)) + p[size:]
		}
	}
	return strings.Join(parts, " ")
}

func formatMatcherTitle(matcher string) string {
	tag := strings.TrimPrefix(strings.TrimSpace(matcher), "@")
	if tag == "" {
		return ""
	}
	return formatWordsTitle(tag)
}

func formatSubdomainTitle(domain string) string {
	sub := strings.Split(domain, ".")[0]
	if title := formatWordsTitle(sub); title != "" {
		return title
	}
	return domain
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
// Returns true if at least one discovery source succeeded.
func (s *Server) reconcileState(force bool) bool {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	cfg := s.envConfig
	mode := cfg.CaddyMode

	// Optimization: when watching Caddyfile in background (!force), check file mtime/size first
	if mode == "file" && !force {
		info, err := os.Stat(cfg.CaddyFilePath)
		if err != nil {
			return false
		}
		if info.ModTime().Equal(s.lastFileModTime) && info.Size() == s.lastFileSize {
			return true
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
		return false
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
		baseID := slugifyCategoryID(catName)
		newID := baseID
		for suffix := 2; ; suffix++ {
			collision := false
			for _, c := range s.state.Categories {
				if c.ID == newID {
					collision = true
					break
				}
			}
			if !collision {
				break
			}
			newID = fmt.Sprintf("%s_%d", baseID, suffix)
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
	return true
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

func (s *Server) handleFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(embeddedLogoSVG)
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

func collectReferencedUploads(st *AppState) map[string]bool {
	refs := make(map[string]bool)
	addRef := func(u string) {
		u = strings.TrimSpace(u)
		if strings.HasPrefix(u, "/uploads/") {
			name := filepath.Base(strings.TrimPrefix(u, "/uploads/"))
			if name != "" && name != "." && name != "/" {
				refs[name] = true
			}
		}
	}
	addRef(st.Settings.BackgroundImage)
	for _, svc := range st.Services {
		if svc != nil {
			addRef(svc.Icon)
		}
	}
	return refs
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

	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	payload, err := json.MarshalIndent(incoming, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed encoding state: " + err.Error()})
		return
	}
	if err := s.saveStatePayloadLocked(payload); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed persisting state: " + err.Error()})
		return
	}

	newRefs := collectReferencedUploads(&incoming)
	s.mu.Lock()
	s.state = incoming
	s.mu.Unlock()

	// Remove any uploaded files in uploadsDir that are not referenced by the saved state
	if entries, readErr := os.ReadDir(s.uploadsDir); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() && !newRefs[entry.Name()] {
				_ = os.Remove(filepath.Join(s.uploadsDir, entry.Name()))
			}
		}
	}

	writeJSON(w, http.StatusOK, incoming)
}

var allowedImageMimes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	".gif":  "image/gif",
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
	if _, ok := allowedImageMimes[ext]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported image format"})
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, (10<<20)+1))
	if err != nil || len(data) == 0 || len(data) > 10<<20 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty or oversized image file"})
		return
	}
	if _, ok := detectImageContentType(data, header.Header.Get("Content-Type"), header.Filename); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file content is not a valid image"})
		return
	}

	randBytes := make([]byte, 4)
	_, _ = rand.Read(randBytes)
	filename := fmt.Sprintf("%d-%s-%s%s", time.Now().Unix(), hex.EncodeToString(randBytes), sanitizeFilename(header.Filename), ext)
	destPath := filepath.Join(s.uploadsDir, filename)

	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed saving uploaded file"})
		return
	}
	s.applyOwnership(destPath)

	writeJSON(w, http.StatusOK, map[string]string{
		"url": "/uploads/" + filename,
	})
}

func setSafeAssetHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
}

func writeFaviconResponse(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	setSafeAssetHeaders(w)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func (s *Server) handleUploadsFile(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/uploads/")
	if rel == "" || strings.Contains(rel, "/") || strings.Contains(rel, "\\") || rel == "." || rel == ".." {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(s.uploadsDir, filepath.Clean(rel))
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	setSafeAssetHeaders(w)
	http.ServeFile(w, r, fullPath)
}

var (
	reLinkTag  = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	reRelAttr  = regexp.MustCompile(`(?is)\brel\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>"']+))`)
	reHrefAttr = regexp.MustCompile(`(?is)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>"']+))`)
)

func attrValue(m []string) string {
	for i := 1; i < len(m); i++ {
		if m[i] != "" {
			return strings.TrimSpace(m[i])
		}
	}
	return ""
}

func extractHTMLIconHrefs(html string) []string {
	var icons []string
	var appleIcons []string
	for _, tag := range reLinkTag.FindAllString(html, -1) {
		relMatch := reRelAttr.FindStringSubmatch(tag)
		hrefMatch := reHrefAttr.FindStringSubmatch(tag)
		if len(relMatch) == 0 || len(hrefMatch) == 0 {
			continue
		}
		rel := strings.ToLower(attrValue(relMatch))
		href := attrValue(hrefMatch)
		if href == "" || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			continue
		}
		tokens := strings.Fields(rel)
		hasIcon := slices.Contains(tokens, "icon")
		hasApple := slices.Contains(tokens, "apple-touch-icon") || slices.Contains(tokens, "apple-touch-icon-precomposed")
		if hasIcon {
			icons = append(icons, href)
		} else if hasApple {
			appleIcons = append(appleIcons, href)
		}
	}
	return append(icons, appleIcons...)
}

func parseMimeType(rawCT string) string {
	mediaType, _, _ := strings.Cut(rawCT, ";")
	return strings.ToLower(strings.TrimSpace(mediaType))
}

func isNonImageHeaderContentType(ct string) bool {
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "text/plain") || strings.HasPrefix(ct, "application/json")
}

// detectImageContentType validates whether data is a supported image and returns its normalized MIME type.
func detectImageContentType(data []byte, rawCT, targetURL string) (string, bool) {
	ct := parseMimeType(rawCT)
	if len(data) == 0 || isNonImageHeaderContentType(ct) {
		return "", false
	}
	trimmed := bytes.TrimSpace(data)
	lowerPrefix := strings.ToLower(string(trimmed[:min(len(trimmed), 256)]))
	if strings.HasPrefix(lowerPrefix, "<!doctype html") || strings.HasPrefix(lowerPrefix, "<html") {
		return "", false
	}
	uLower := strings.ToLower(targetURL)
	// ICO magic bytes: 00 00 01 00 or 00 00 02 00
	isICO := len(data) >= 4 && data[0] == 0 && data[1] == 0 && (data[2] == 1 || data[2] == 2) && data[3] == 0

	if strings.HasPrefix(ct, "image/") {
		return ct, true
	}
	if strings.Contains(lowerPrefix, "<svg") {
		return "image/svg+xml", true
	}
	if isICO {
		return "image/x-icon", true
	}
	sniff := parseMimeType(http.DetectContentType(data))
	if strings.HasPrefix(sniff, "image/") {
		return sniff, true
	}
	for ext, mimeType := range allowedImageMimes {
		if strings.HasSuffix(uLower, ext) && !strings.HasPrefix(sniff, "text/") {
			return mimeType, true
		}
	}
	return "", false
}

func (s *Server) fetchImageCandidate(targetURL string) ([]byte, string, bool) {
	req, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", "CaddyHome/1.0")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	resp, err := s.faviconClient.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", false
	}
	ct := resp.Header.Get("Content-Type")
	if isNonImageHeaderContentType(parseMimeType(ct)) {
		return nil, "", false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2 MB max
	if err != nil || len(data) == 0 {
		return nil, "", false
	}
	finalURL := targetURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	normCT, ok := detectImageContentType(data, ct, finalURL)
	if !ok {
		return nil, "", false
	}
	return data, normCT, true
}

func (s *Server) discoverFaviconFromBase(baseURL string) ([]byte, string, bool) {
	parsedBase, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsedBase.Scheme != "http" && parsedBase.Scheme != "https") || parsedBase.Host == "" {
		return nil, "", false
	}
	origin := parsedBase.Scheme + "://" + parsedBase.Host
	tried := make(map[string]bool, 8)
	tryCandidate := func(u string) ([]byte, string, bool) {
		if u == "" || tried[u] {
			return nil, "", false
		}
		tried[u] = true
		return s.fetchImageCandidate(u)
	}

	// 1. Try /favicon.ico directly
	if data, ct, ok := tryCandidate(origin + "/favicon.ico"); ok {
		return data, ct, true
	}

	// 2. Fetch root HTML page (following redirects) and inspect <link rel="icon" ...>
	req, err := http.NewRequest(http.MethodGet, baseURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "CaddyHome/1.0")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		if resp, err := s.faviconClient.Do(req); err == nil {
			ct := strings.ToLower(resp.Header.Get("Content-Type"))
			var bodyBytes []byte
			if resp.StatusCode < 500 && (ct == "" || strings.Contains(ct, "html") || strings.Contains(ct, "xml") || strings.HasPrefix(ct, "text/")) {
				bodyBytes, _ = io.ReadAll(io.LimitReader(resp.Body, 256<<10))
			}
			resp.Body.Close()
			if len(bodyBytes) > 0 {
				pageBase := parsedBase
				if resp.Request != nil && resp.Request.URL != nil {
					pageBase = resp.Request.URL
				}
				for _, href := range extractHTMLIconHrefs(string(bodyBytes)) {
					refURL, err := pageBase.Parse(href)
					if err != nil {
						continue
					}
					if data, ct, ok := tryCandidate(refURL.String()); ok {
						return data, ct, true
					}
				}
			}
		}
	}

	// 3. Try common fallback paths (e.g. Home Assistant /static/icons/favicon.ico, /favicon.svg, /favicon.png, /apple-touch-icon.png)
	for _, path := range []string{
		"/static/icons/favicon.ico",
		"/favicon.svg",
		"/favicon.png",
		"/apple-touch-icon.png",
	} {
		if data, ct, ok := tryCandidate(origin + path); ok {
			return data, ct, true
		}
	}

	return nil, "", false
}

func (s *Server) isAllowedFaviconDomain(domain string) (*Service, bool) {
	s.mu.RLock()
	svc, ok := s.state.Services[domain]
	if !ok || svc == nil {
		s.mu.RUnlock()
		return nil, false
	}
	cp := *svc
	cp.Upstreams = slices.Clone(svc.Upstreams)
	s.mu.RUnlock()
	return &cp, true
}

func (s *Server) finishFaviconInflight(key string, flight *faviconInflight, entry cachedFavicon, now time.Time) {
	flight.entry = entry
	s.faviconMu.Lock()
	if len(s.faviconCache) > 256 {
		for k, v := range s.faviconCache {
			if now.After(v.expiresAt) {
				delete(s.faviconCache, k)
			}
		}
	}
	s.faviconCache[key] = entry
	delete(s.faviconInflight, key)
	close(flight.done)
	s.faviconMu.Unlock()
}

func (s *Server) handleServiceFavicon(w http.ResponseWriter, r *http.Request) {
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		http.NotFound(w, r)
		return
	}
	svc, ok := s.isAllowedFaviconDomain(domain)
	if !ok {
		http.NotFound(w, r)
		return
	}

	var candidates []string
	addCandidate := func(u string) {
		u = strings.TrimSpace(u)
		if u != "" && !slices.Contains(candidates, u) {
			candidates = append(candidates, u)
		}
	}
	if svc.OverrideLanURL != nil && strings.TrimSpace(*svc.OverrideLanURL) != "" {
		addCandidate(*svc.OverrideLanURL)
	}
	for _, up := range svc.Upstreams {
		addCandidate(up)
	}
	if svc.ExternalURL != "" {
		addCandidate(svc.ExternalURL)
	} else if svc.Domain != "" {
		addCandidate("https://" + svc.Domain)
	}

	cacheKey := domain + "|" + strings.Join(candidates, ",")
	now := time.Now()

	s.faviconMu.RLock()
	cached, found := s.faviconCache[cacheKey]
	s.faviconMu.RUnlock()
	if found && now.Before(cached.expiresAt) {
		if cached.notFound {
			http.NotFound(w, r)
			return
		}
		writeFaviconResponse(w, cached.contentType, cached.data)
		return
	}

	s.faviconMu.Lock()
	if cached, found = s.faviconCache[cacheKey]; found && now.Before(cached.expiresAt) {
		s.faviconMu.Unlock()
		if cached.notFound {
			http.NotFound(w, r)
			return
		}
		writeFaviconResponse(w, cached.contentType, cached.data)
		return
	}
	if flight, inflight := s.faviconInflight[cacheKey]; inflight {
		s.faviconMu.Unlock()
		<-flight.done
		if flight.entry.notFound {
			http.NotFound(w, r)
			return
		}
		writeFaviconResponse(w, flight.entry.contentType, flight.entry.data)
		return
	}
	flight := &faviconInflight{done: make(chan struct{})}
	s.faviconInflight[cacheKey] = flight
	s.faviconMu.Unlock()

	for _, base := range candidates {
		if data, ct, ok := s.discoverFaviconFromBase(base); ok {
			s.finishFaviconInflight(cacheKey, flight, cachedFavicon{
				data:        data,
				contentType: ct,
				expiresAt:   now.Add(6 * time.Hour),
			}, now)
			writeFaviconResponse(w, ct, data)
			return
		}
	}

	s.finishFaviconInflight(cacheKey, flight, cachedFavicon{
		notFound:  true,
		expiresAt: now.Add(2 * time.Minute),
	}, now)
	http.NotFound(w, r)
}

func (s *Server) startBackgroundSync(initialSuccess bool) {
	intervalSec := s.envConfig.SyncIntervalSeconds
	if intervalSec < 5 {
		intervalSec = 60
	}
	go func() {
		// If initial sync failed (e.g. Caddy Admin API not ready yet on container startup),
		// retry with short backoff before falling into the regular ticker loop.
		if !initialSuccess {
			for _, waitSec := range []int{2, 4, 8, 15} {
				time.Sleep(time.Duration(waitSec) * time.Second)
				if s.reconcileState(false) {
					break
				}
			}
		}
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
	mux.HandleFunc("GET /favicon.svg", s.handleFavicon)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /uploads/", s.handleUploadsFile)
	mux.HandleFunc("GET /api/favicon", s.handleServiceFavicon)
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
	initialOK := srv.reconcileState(true)
	srv.startBackgroundSync(initialOK)

	addr := ":" + port
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("CaddyHome listening on %s (config: %s)", addr, configPath)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
