package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
