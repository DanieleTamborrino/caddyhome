package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeUpstream(t *testing.T) {
	cases := []struct {
		raw      string
		lanIP    string
		hasTLS   bool
		expected string
	}{
		{"192.168.1.50:8096", "192.168.1.10", false, "http://192.168.1.50:8096"},
		{"jellyfin:8096", "192.168.1.10", false, "http://192.168.1.10:8096"},
		{"localhost:3000", "192.168.1.10", false, "http://localhost:3000"},
		{"https://192.168.1.2:8006", "192.168.1.10", false, "https://192.168.1.2:8006"},
		{"192.168.1.2:8006", "192.168.1.10", false, "https://192.168.1.2:8006"},
		{"proxmox:9443", "192.168.1.10", false, "https://192.168.1.10:9443"},
		{"10.0.0.5:8080", "192.168.1.10", true, "https://10.0.0.5:8080"},
	}
	for _, tc := range cases {
		got := normalizeUpstream(tc.raw, tc.lanIP, tc.hasTLS)
		if got != tc.expected {
			t.Errorf("normalizeUpstream(%q, %q, %v) = %q; want %q", tc.raw, tc.lanIP, tc.hasTLS, got, tc.expected)
		}
	}
}

func TestParseCaddyfileContent(t *testing.T) {
	sample := `
# Global block
{
	email admin@example.com
}

# @name Jellyfin Media
# @category Media
# @icon jellyfin
jellyfin.example.com {
	reverse_proxy jellyfin:8096
}

pve.example.com {
	reverse_proxy 192.168.1.2:8006 {
		transport http {
			tls_insecure_skip_verify
		}
	}
}

a.example.com, b.example.com {
	reverse_proxy 10.0.0.1:8080 10.0.0.2:8080
}

single.example.com { reverse_proxy 10.0.0.9:3000 }

*.example.com {
	# @name Sonarr TV
	# @category Media
	@sonarr host sonarr.example.com
	handle @sonarr {
		reverse_proxy sonarr:8989
	}

	@vault host vault.example.com
	handle @vault {
		reverse_proxy 192.168.1.99:8222 {
			transport http {
				tls
			}
		}
	}
}
`
	res := parseCaddyfileContent(sample, "192.168.1.10")

	if len(res) != 7 {
		t.Fatalf("expected 7 discovered services, got %d", len(res))
	}

	jf := res["jellyfin.example.com"]
	if jf == nil || jf.MagicName != "Jellyfin Media" || jf.MagicCategory != "Media" || jf.MagicIcon != "https://cdn.jsdelivr.net/gh/walkxcode/dashboard-icons/png/jellyfin.png" {
		t.Fatalf("unexpected jellyfin metadata: %+v", jf)
	}
	if !slices.Equal(jf.Upstreams, []string{"http://192.168.1.10:8096"}) {
		t.Fatalf("unexpected jellyfin upstreams: %v", jf.Upstreams)
	}

	pve := res["pve.example.com"]
	if pve == nil || !slices.Equal(pve.Upstreams, []string{"https://192.168.1.2:8006"}) {
		t.Fatalf("unexpected pve upstreams: %+v", pve)
	}

	sonarr := res["sonarr.example.com"]
	if sonarr == nil || sonarr.MagicName != "Sonarr TV" || !slices.Equal(sonarr.Upstreams, []string{"http://192.168.1.10:8989"}) {
		t.Fatalf("unexpected sonarr entry: %+v", sonarr)
	}

	vault := res["vault.example.com"]
	if vault == nil || !slices.Equal(vault.Upstreams, []string{"https://192.168.1.99:8222"}) {
		t.Fatalf("unexpected vault entry: %+v", vault)
	}
}

func TestParseCaddyAPI(t *testing.T) {
	mockJSON := `{
		"srv0": {
			"routes": [
				{
					"match": [{"host": ["*.example.com"]}],
					"handle": [
						{
							"handler": "subroute",
							"routes": [
								{
									"match": [{"host": ["radarr.example.com"]}],
									"handle": [
										{
											"handler": "reverse_proxy",
											"upstreams": [{"dial": "radarr:7878"}]
										}
									]
								},
								{
									"match": [{"host": ["unifi.example.com"]}],
									"handle": [
										{
											"handler": "reverse_proxy",
											"transport": {"protocol": "http", "tls": {}},
											"upstreams": [{"dial": "192.168.1.5:8443"}]
										}
									]
								}
							]
						}
					]
				}
			]
		}
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/apps/http/servers" {
			t.Errorf("unexpected API request path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockJSON))
	}))
	defer ts.Close()

	srv, err := NewServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := srv.parseCaddyAPI(ts.URL+"/config/", "192.168.1.10")
	if err != nil {
		t.Fatalf("parseCaddyAPI failed: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 services from API, got %d", len(res))
	}
	if !slices.Equal(res["radarr.example.com"].Upstreams, []string{"http://192.168.1.10:7878"}) {
		t.Fatalf("unexpected radarr upstreams: %v", res["radarr.example.com"].Upstreams)
	}
	if !slices.Equal(res["unifi.example.com"].Upstreams, []string{"https://192.168.1.5:8443"}) {
		t.Fatalf("unexpected unifi upstreams: %v", res["unifi.example.com"].Upstreams)
	}
}

func TestReconcileAndEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	caddyfilePath := filepath.Join(tmpDir, "Caddyfile")

	err := os.WriteFile(caddyfilePath, []byte(`
grafana.example.com {
	reverse_proxy grafana:3000
}
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CADDY_MODE", "file")
	t.Setenv("CADDYFILE_PATH", caddyfilePath)
	t.Setenv("DEFAULT_LAN_IP", "192.168.1.25")

	srv, err := NewServer(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.reconcileState(true)

	handler := srv.routes()

	// 1. Check GET /api/state
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state status = %d", rec.Code)
	}

	var st AppState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	graf := st.Services["grafana.example.com"]
	if graf == nil || graf.CustomName != "Grafana" || graf.Icon != "" || !graf.Active {
		t.Fatalf("unexpected grafana service: %+v", graf)
	}
	if !slices.Equal(graf.Upstreams, []string{"http://192.168.1.25:3000"}) {
		t.Fatalf("unexpected grafana upstreams: %v", graf.Upstreams)
	}

	// 2. Customize user fields via PUT /api/state and ensure sync does NOT overwrite them
	graf.CustomName = "My Custom Grafana"
	graf.Icon = "/uploads/custom-grafana.png"
	graf.Hidden = true
	st.Settings.Theme = "catppuccin"
	st.Settings.BackgroundImage = "/uploads/bg.jpg"

	putBody, _ := json.Marshal(st)
	putReq := httptest.NewRequest(http.MethodPut, "/api/state", bytes.NewReader(putBody))
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT /api/state status = %d", putRec.Code)
	}

	// Update Caddyfile upstream and add a new domain, then sync
	_ = os.WriteFile(caddyfilePath, []byte(`
grafana.example.com {
	reverse_proxy 192.168.1.99:3000
}
plex.example.com {
	reverse_proxy 192.168.1.50:32400
}
`), 0o644)

	syncReq := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	syncRec := httptest.NewRecorder()
	handler.ServeHTTP(syncRec, syncReq)
	if syncRec.Code != http.StatusOK {
		t.Fatalf("POST /api/sync status = %d", syncRec.Code)
	}

	_ = json.Unmarshal(syncRec.Body.Bytes(), &st)
	grafAfter := st.Services["grafana.example.com"]
	if grafAfter.CustomName != "My Custom Grafana" || grafAfter.Icon != "/uploads/custom-grafana.png" || !grafAfter.Hidden {
		t.Fatalf("reconcile overwrote user customizations: %+v", grafAfter)
	}
	if !slices.Equal(grafAfter.Upstreams, []string{"http://192.168.1.99:3000"}) {
		t.Fatalf("reconcile did not update upstreams: %v", grafAfter.Upstreams)
	}

	// 3. Test file upload endpoint
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "logo.png")
	_, _ = fw.Write([]byte("fake-png-bytes"))
	_ = mw.Close()

	upReq := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upRec := httptest.NewRecorder()
	handler.ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusOK {
		t.Fatalf("POST /api/upload status = %d: %s", upRec.Code, upRec.Body.String())
	}
}

func TestDefaultModeAndEnvOnlyConfig(t *testing.T) {
	t.Setenv("CADDY_MODE", "")
	def := defaultSettings()
	if def.Title != "CaddyHome" {
		t.Fatalf("expected default Title 'CaddyHome', got %q", def.Title)
	}
	envCfg := loadEnvConfig()
	if envCfg.CaddyMode != "api" {
		t.Fatalf("expected default CaddyMode 'api', got %q", envCfg.CaddyMode)
	}

	// Ensure persisted config.json never stores docker-compose environment settings,
	// and strips legacy env keys if an older config.json already has them on disk.
	tmpCfg := filepath.Join(t.TempDir(), "config.json")
	legacyJSON := `{"settings":{"title":"CaddyHome","theme":"dark","caddy_api_url":"http://caddy:2019/config/","caddy_mode":"api"},"categories":[],"services":{}}`
	if err := os.WriteFile(tmpCfg, []byte(legacyJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CADDY_MODE", "both") // invalid/legacy mode should fall back to "api"
	t.Setenv("CADDY_API_URL", "http://localhost:2019/config/")
	t.Setenv("PUID", "3000")
	t.Setenv("PGID", "3000")
	srv, err := NewServer(tmpCfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.envConfig.CaddyMode != "api" {
		t.Fatalf("expected legacy 'both' to normalize to 'api', got %q", srv.envConfig.CaddyMode)
	}
	if srv.envConfig.CaddyAPIURL != "http://localhost:2019/config/" {
		t.Fatalf("expected CADDY_API_URL from env, got %q", srv.envConfig.CaddyAPIURL)
	}
	if srv.envConfig.PUID != 3000 || srv.envConfig.PGID != 3000 {
		t.Fatalf("expected PUID/PGID 3000/3000, got %d/%d", srv.envConfig.PUID, srv.envConfig.PGID)
	}

	rawDisk, err := os.ReadFile(tmpCfg)
	if err != nil {
		t.Fatal(err)
	}
	diskStr := string(rawDisk)
	for _, forbiddenKey := range []string{"caddy_mode", "caddy_file_path", "caddy_api_url", "default_lan_ip", "sync_interval_seconds", "puid", "pgid"} {
		if bytes.Contains(rawDisk, []byte(forbiddenKey)) {
			t.Fatalf("config.json must not persist env setting %q, got:\n%s", forbiddenKey, diskStr)
		}
	}
}

func TestMatcherNamingAndParserEnhancements(t *testing.T) {
	sample := `
# @category Cycling
*.domain.com {
	tls internal

	@dreeve-zwift host dreeve-zwift.domain.com
	handle @dreeve-zwift {
		@api path /api/*
		handle @api {
			reverse_proxy 192.168.1.50:3001
		}
		reverse_proxy /v1.0/* 192.168.1.50:3000
	}

	@my_ha_instance host home.domain.com
	reverse_proxy @my_ha_instance 192.168.1.60:8123

	# @name Custom Override Name
	@overridden-tag host sub.domain.com
	handle @overridden-tag {
		reverse_proxy {
			to 192.168.1.70:9000
			transport http {
				tls_insecure_skip_verify
			}
		}
	}

	@inline-tls host secure.domain.com
	handle @inline-tls { reverse_proxy 192.168.1.80:8080 { transport http { tls } } }

	@über-panel {
		host uber.domain.com
	}
	handle @über-panel {
		reverse_proxy 192.168.1.90:4000
	}
}

concrete.domain.com {
	@ws header Connection *Upgrade*
	reverse_proxy @ws 192.168.1.95:5000
	reverse_proxy /api/* 192.168.1.95:5001
}
`
	res := parseCaddyfileContent(sample, "192.168.1.10")
	if len(res) != 6 {
		t.Fatalf("expected 6 services, got %d", len(res))
	}

	zwift := res["dreeve-zwift.domain.com"]
	if zwift == nil || zwift.MagicName != "Dreeve Zwift" || zwift.MagicCategory != "Cycling" {
		t.Fatalf("expected @dreeve-zwift to produce initial name 'Dreeve Zwift' and category 'Cycling', got %+v", zwift)
	}
	// Site-level `tls internal` must NOT force upstream to https, and `/v1.0/*` path matcher must not become an upstream
	if !slices.Equal(zwift.Upstreams, []string{"http://192.168.1.50:3001", "http://192.168.1.50:3000"}) {
		t.Fatalf("unexpected zwift upstreams: %v", zwift.Upstreams)
	}

	ha := res["home.domain.com"]
	if ha == nil || ha.MagicName != "My Ha Instance" || ha.MagicCategory != "Cycling" || !slices.Equal(ha.Upstreams, []string{"http://192.168.1.60:8123"}) {
		t.Fatalf("expected @my_ha_instance to name home.domain.com 'My Ha Instance', got %+v", ha)
	}

	over := res["sub.domain.com"]
	if over == nil || over.MagicName != "Custom Override Name" || !slices.Equal(over.Upstreams, []string{"https://192.168.1.70:9000"}) {
		t.Fatalf("expected # @name to override @overridden-tag and multi-line `to` + `tls` to work, got %+v", over)
	}

	sec := res["secure.domain.com"]
	if sec == nil || sec.MagicName != "Inline Tls" || !slices.Equal(sec.Upstreams, []string{"https://192.168.1.80:8080"}) {
		t.Fatalf("expected single-line handle with transport tls to produce https upstream and 'Inline Tls', got %+v", sec)
	}

	uber := res["uber.domain.com"]
	if uber == nil || uber.MagicName != "Über Panel" || !slices.Equal(uber.Upstreams, []string{"http://192.168.1.90:4000"}) {
		t.Fatalf("expected multi-line @über-panel { host ... } to produce 'Über Panel', got %+v", uber)
	}

	conc := res["concrete.domain.com"]
	if conc == nil || !slices.Equal(conc.Upstreams, []string{"http://192.168.1.95:5000", "http://192.168.1.95:5001"}) {
		t.Fatalf("expected non-host matcher @ws and path matcher /api/* on concrete domain to preserve upstreams, got %+v", conc)
	}
}

func TestStateCategoryIntegrityAndUploadSecurity(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	caddyfilePath := filepath.Join(tmpDir, "Caddyfile")

	// Two categories with symbols that would otherwise slugify to the same base ID
	err := os.WriteFile(caddyfilePath, []byte(`
# @category C++
cpp.example.com {
	reverse_proxy 192.168.1.10:8081
}

# @category C#
csharp.example.com {
	reverse_proxy 192.168.1.10:8082
}

*.example.com {
	@dreeve-zwift host dreeve-zwift.example.com
	handle @dreeve-zwift {
		reverse_proxy 192.168.1.10:8083
	}
}
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CADDY_MODE", "file")
	t.Setenv("CADDYFILE_PATH", caddyfilePath)

	srv, err := NewServer(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !srv.reconcileState(true) {
		t.Fatalf("expected reconcileState to succeed")
	}

	srv.mu.RLock()
	cppCat := srv.state.Services["cpp.example.com"].CategoryID
	csCat := srv.state.Services["csharp.example.com"].CategoryID
	zwiftName := srv.state.Services["dreeve-zwift.example.com"].CustomName
	srv.mu.RUnlock()

	if cppCat == csCat {
		t.Fatalf("expected distinct category IDs for 'C++' and 'C#', both got %q", cppCat)
	}
	if zwiftName != "Dreeve Zwift" {
		t.Fatalf("expected reconciled CustomName 'Dreeve Zwift', got %q", zwiftName)
	}

	handler := srv.routes()

	// Verify /uploads/ directory listing is disabled (404)
	reqDir := httptest.NewRequest(http.MethodGet, "/uploads/", nil)
	recDir := httptest.NewRecorder()
	handler.ServeHTTP(recDir, reqDir)
	if recDir.Code != http.StatusNotFound {
		t.Fatalf("expected GET /uploads/ to return 404, got %d", recDir.Code)
	}

	uploadFile := func(name, content string) string {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()

		upReq := httptest.NewRequest(http.MethodPost, "/api/upload", &buf)
		upReq.Header.Set("Content-Type", mw.FormDataContentType())
		upRec := httptest.NewRecorder()
		handler.ServeHTTP(upRec, upReq)
		if upRec.Code != http.StatusOK {
			t.Fatalf("upload failed: %d", upRec.Code)
		}
		var upResp map[string]string
		_ = json.Unmarshal(upRec.Body.Bytes(), &upResp)
		return upResp["url"]
	}

	// Upload two files: discardedURL (never referenced in state) and uploadedURL (assigned to service)
	discardedURL := uploadFile("discarded.png", "fake-png")
	uploadedURL := uploadFile("icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"></svg>`)

	// Check security headers on served upload
	getUpReq := httptest.NewRequest(http.MethodGet, uploadedURL, nil)
	getUpRec := httptest.NewRecorder()
	handler.ServeHTTP(getUpRec, getUpReq)
	if getUpRec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d", uploadedURL, getUpRec.Code)
	}
	if getUpRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options: nosniff")
	}
	if getUpRec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("expected Content-Security-Policy header on uploads")
	}

	// Assign uploadedURL and a non-existent category ID via PUT /api/state
	srv.mu.RLock()
	stCopy := srv.state
	srv.mu.RUnlock()
	stCopy.Services["cpp.example.com"].Icon = uploadedURL
	stCopy.Services["cpp.example.com"].CategoryID = "cat_does_not_exist"

	body1, _ := json.Marshal(stCopy)
	put1 := httptest.NewRequest(http.MethodPut, "/api/state", bytes.NewReader(body1))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, put1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("PUT 1 status = %d", rec1.Code)
	}
	var afterPut1 AppState
	_ = json.Unmarshal(rec1.Body.Bytes(), &afterPut1)
	if afterPut1.Services["cpp.example.com"].CategoryID != "cat_uncat" {
		t.Fatalf("expected orphaned CategoryID to be re-homed to cat_uncat, got %q", afterPut1.Services["cpp.example.com"].CategoryID)
	}

	// Verify discardedURL (never referenced in state) was already cleaned up by PUT 1
	getDiscReq := httptest.NewRequest(http.MethodGet, discardedURL, nil)
	recDisc := httptest.NewRecorder()
	handler.ServeHTTP(recDisc, getDiscReq)
	if recDisc.Code != http.StatusNotFound {
		t.Fatalf("expected discarded upload %s to be deleted on PUT 1 (404), got %d", discardedURL, recDisc.Code)
	}

	// Now clear the icon and save state -> uploadedURL should also be removed from disk
	afterPut1.Services["cpp.example.com"].Icon = ""
	body2, _ := json.Marshal(afterPut1)
	put2 := httptest.NewRequest(http.MethodPut, "/api/state", bytes.NewReader(body2))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, put2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("PUT 2 status = %d", rec2.Code)
	}

	getUpRec2 := httptest.NewRequest(http.MethodGet, uploadedURL, nil)
	recUp2 := httptest.NewRecorder()
	handler.ServeHTTP(recUp2, getUpRec2)
	if recUp2.Code != http.StatusNotFound {
		t.Fatalf("expected unreferenced upload %s to be deleted (404), got %d", uploadedURL, recUp2.Code)
	}
}

func TestFrontendJSLogic(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed, skipping frontend JS behavioral test")
	}

	htmlStr := string(embeddedIndexHTML)
	scriptStart := strings.LastIndex(htmlStr, "<script>")
	scriptEnd := strings.LastIndex(htmlStr, "</script>")
	if scriptStart == -1 || scriptEnd == -1 || scriptEnd <= scriptStart {
		t.Fatalf("could not locate embedded <script> block in index.html")
	}
	appScript := htmlStr[scriptStart+len("<script>") : scriptEnd]

	testHarness := appScript + `
const assert = require("assert");

// Mock browser globals
global.localStorage = {
  store: {},
  getItem(k) { return this.store[k] || null; },
  setItem(k, v) { this.store[k] = v; }
};
let currentThemeAttr = "dark";
let mockSections = [];
let mockGrids = [];
global.document = {
  title: "",
  documentElement: {
    setAttribute(k, v) { if (k === "data-theme") currentThemeAttr = v; },
    getAttribute(k) { return k === "data-theme" ? currentThemeAttr : null; }
  },
  querySelectorAll(sel) {
    if (sel === "#categories-root > section[data-category-id]") return mockSections;
    if (sel === ".service-grid[data-category-id]") return mockGrids;
    return [];
  }
};

const app = caddyHomeApp();
app.persistState = async () => {};
app.$nextTick = (fn) => fn();
app.initSortable = () => {};

// 1. Test getLanUrl does not fall back to external_url when no upstreams/override
assert.strictEqual(app.getLanUrl({ domain: "a.com", external_url: "https://a.com", upstreams: [] }), "");
assert.strictEqual(app.getLanUrl({ domain: "a.com", external_url: "https://a.com", upstreams: ["http://10.0.0.1:8080"] }), "http://10.0.0.1:8080");

// 2. Test getIconUrl uses /api/favicon proxy and handleIconError falls back to direct external, LAN, then SVG
const svcWithLan = { domain: "a.com", external_url: "https://a.com", upstreams: ["http://10.0.0.1:8080"], icon: "" };
assert.strictEqual(app.getIconUrl(svcWithLan), "/api/favicon?domain=a.com");
const img = { src: "/api/favicon?domain=a.com", dataset: {} };
app.handleIconError({ target: img }, svcWithLan);
assert.strictEqual(img.src, "https://a.com/favicon.ico");
assert.strictEqual(img.dataset.triedExternal, "1");
app.handleIconError({ target: img }, svcWithLan);
assert.strictEqual(img.src, "http://10.0.0.1:8080/favicon.ico");
assert.strictEqual(img.dataset.triedLan, "1");
// Third error falls back to SVG
app.handleIconError({ target: img }, svcWithLan);
assert.strictEqual(img.src, FALLBACK_ICON_SVG);
// Changing override_lan_url resets state and retries external then new LAN favicon
svcWithLan.override_lan_url = "http://10.0.0.2:9090";
app.handleIconError({ target: img }, svcWithLan);
assert.strictEqual(img.src, "https://a.com/favicon.ico");
app.handleIconError({ target: img }, svcWithLan);
assert.strictEqual(img.src, "http://10.0.0.2:9090/favicon.ico");

// 3. Test two-pass syncOrderFromDOM when dragging svcA from cat_main (which has a hidden card) to cat_media
app.state = {
  settings: { title: "CaddyHome", theme: "dark" },
  categories: [
    { id: "cat_main", name: "Main", order: 0 },
    { id: "cat_media", name: "Media", order: 1 },
    { id: "cat_uncat", name: "Uncat", order: 99 }
  ],
  services: {
    "a.com": { domain: "a.com", category_id: "cat_main", order: 0, hidden: false },
    "hidden.com": { domain: "hidden.com", category_id: "cat_main", order: 1, hidden: true },
    "b.com": { domain: "b.com", category_id: "cat_media", order: 0, hidden: false }
  }
};
mockSections = [
  { getAttribute: () => "cat_main" },
  { getAttribute: () => "cat_media" },
  { getAttribute: () => "cat_uncat" }
];
// Simulate DOM after dragging a.com from cat_main into cat_media (hidden.com is not in DOM because showHidden=false)
mockGrids = [
  {
    getAttribute: () => "cat_main",
    querySelectorAll: () => []
  },
  {
    getAttribute: () => "cat_media",
    querySelectorAll: () => [
      { getAttribute: () => "b.com" },
      { getAttribute: () => "a.com" }
    ]
  },
  {
    getAttribute: () => "cat_uncat",
    querySelectorAll: () => []
  }
];
app.syncOrderFromDOM();
assert.strictEqual(app.state.services["a.com"].category_id, "cat_media");
assert.strictEqual(app.state.services["b.com"].order, 0);
assert.strictEqual(app.state.services["a.com"].order, 1);
assert.strictEqual(app.state.services["hidden.com"].category_id, "cat_main");
assert.strictEqual(app.state.services["hidden.com"].order, 0);

// 4. Test closeAppearanceModal restores saved theme
app.state.settings.theme = "dark";
app.openAppearanceModal();
app.applyTheme("light");
assert.strictEqual(currentThemeAttr, "light");
app.closeAppearanceModal();
assert.strictEqual(currentThemeAttr, "dark");
assert.strictEqual(app.appearanceModal.open, false);

// 5. Test deleteCategory assigns non-colliding orders in cat_uncat
app.state.services["u.com"] = { domain: "u.com", category_id: "cat_uncat", order: 0, hidden: false };
app.deleteCategory("cat_media").then(() => {
  assert.strictEqual(app.state.services["b.com"].category_id, "cat_uncat");
  assert.strictEqual(app.state.services["a.com"].category_id, "cat_uncat");
  assert.strictEqual(app.state.services["u.com"].order, 0);
  assert.strictEqual(app.state.services["b.com"].order, 1);
  assert.strictEqual(app.state.services["a.com"].order, 2);
});
`

	cmd := exec.Command(nodeBin, "-e", testHarness)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Frontend JS test failed: %v\nOutput:\n%s", err, string(out))
	}
}

func TestServiceFaviconProxyDiscovery(t *testing.T) {
	// Simulate Home Assistant: /favicon.ico returns 404, root / serves HTML with <link rel='shortcut icon' href='/static/icons/favicon.ico'>
	fakeICO := []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10}
	haServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/favicon.ico":
			http.NotFound(w, r)
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><link rel="shortcut icon" href="/static/icons/favicon.ico"><link rel="mask-icon" href="/static/icons/favicon-mask.svg"></head><body>Home Assistant</body></html>`))
		case "/static/icons/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write(fakeICO)
		default:
			http.NotFound(w, r)
		}
	}))
	defer haServer.Close()

	srv, err := NewServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.state.Services["homeassistant.orangebastion.com"] = &Service{
		Domain:      "homeassistant.orangebastion.com",
		CustomName:  "Homeassistant",
		ExternalURL: "https://homeassistant.orangebastion.com",
		Upstreams:   []string{haServer.URL},
		Active:      true,
	}
	srv.mu.Unlock()

	handler := srv.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/favicon?domain=homeassistant.orangebastion.com", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected GET /api/favicon to return 200, got %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), fakeICO) {
		t.Fatalf("expected favicon payload %v, got %v", fakeICO, rec.Body.Bytes())
	}
	if rec.Header().Get("Content-Type") != "image/x-icon" {
		t.Fatalf("expected Content-Type image/x-icon, got %q", rec.Header().Get("Content-Type"))
	}

	// Unknown domain must return 404 (prevents open SSRF)
	reqUnknown := httptest.NewRequest(http.MethodGet, "/api/favicon?domain=unknown.example.com", nil)
	recUnknown := httptest.NewRecorder()
	handler.ServeHTTP(recUnknown, reqUnknown)
	if recUnknown.Code != http.StatusNotFound {
		t.Fatalf("expected unknown domain to return 404, got %d", recUnknown.Code)
	}
}



