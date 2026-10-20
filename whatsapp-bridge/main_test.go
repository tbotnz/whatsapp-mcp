package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                      "report.pdf",
		"../../../etc/passwd":             "passwd",
		"..\\..\\windows\\system.ini":     "system.ini",
		"/etc/shadow":                     "shadow",
		"..":                              "fallback",
		".":                               "fallback",
		"":                                "fallback",
		"evil\x00name.txt":                "evilname.txt",
		"a\nb.txt":                        "ab.txt",
		"weird;$(rm -rf ~).txt":           "weird__(rm -rf _).txt",
		"résumé.pdf":                      "résumé.pdf",
		"   .hidden   ":                   "hidden",
		strings.Repeat("x", 300) + ".pdf": strings.Repeat("x", 196) + ".pdf",
	}
	for in, want := range cases {
		got := sanitizeFilename(in, "fallback")
		if got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "/\\\x00") {
			t.Errorf("sanitizeFilename(%q) = %q contains a path separator", in, got)
		}
	}
}

func TestSafePathComponent(t *testing.T) {
	cases := map[string]string{
		"123@s.whatsapp.net":   "123@s.whatsapp.net",
		"123-456@g.us":         "123-456@g.us",
		"../../etc":            "_.._etc",
		"..":                   "_",
		"a/b/c":                "a_b_c",
		"123:4@s.whatsapp.net": "123_4@s.whatsapp.net",
	}
	for in, want := range cases {
		if got := safePathComponent(in); got != want {
			t.Errorf("safePathComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsWithinDir(t *testing.T) {
	dir := "/srv/store"
	yes := []string{"/srv/store", "/srv/store/a", "/srv/store/a/b.txt"}
	no := []string{"/srv", "/srv/store2", "/srv/storex/a", "/etc/passwd", "/srv/store/../other"}
	for _, p := range yes {
		if !isWithinDir(filepath.Clean(p), dir) {
			t.Errorf("expected %q within %q", p, dir)
		}
	}
	for _, p := range no {
		if isWithinDir(filepath.Clean(p), dir) {
			t.Errorf("expected %q NOT within %q", p, dir)
		}
	}
}

func TestParseRecipient(t *testing.T) {
	good := []string{"64211234567", "1234567@s.whatsapp.net", "123-456@g.us", "999@lid"}
	for _, r := range good {
		if _, err := parseRecipient(r); err != nil {
			t.Errorf("parseRecipient(%q) unexpected error: %v", r, err)
		}
	}
	bad := []string{"", "+64211234567", "abc", "123", "1234567@example.com", "@s.whatsapp.net", "123 456", "1;2"}
	for _, r := range bad {
		if _, err := parseRecipient(r); err == nil {
			t.Errorf("parseRecipient(%q) expected error", r)
		}
	}
}

func newTestConfig(t *testing.T) *SecurityConfig {
	t.Helper()
	store := t.TempDir()
	real, err := filepath.EvalSymlinks(store)
	if err != nil {
		t.Fatal(err)
	}
	return &SecurityConfig{
		StoreDir:         real,
		Token:            strings.Repeat("a", 64),
		AllowedMediaDirs: []string{real},
		MaxMediaBytes:    1024,
		BindAddr:         "127.0.0.1:0",
		EnforceLoopback:  true,
		limiter:          newRateLimiter(2, time.Minute),
	}
}

func TestResolveMediaPath(t *testing.T) {
	sc := newTestConfig(t)
	ok := filepath.Join(sc.StoreDir, "ok.jpg")
	if err := os.WriteFile(ok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(sc.StoreDir, "big.bin")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sc.StoreDir, "link.jpg")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if got, err := sc.resolveMediaPath(ok); err != nil || got != ok {
		t.Errorf("allowed file rejected: %v (%q)", err, got)
	}
	if _, err := sc.resolveMediaPath("relative.jpg"); err == nil {
		t.Error("relative path accepted")
	}
	if _, err := sc.resolveMediaPath(outside); err == nil {
		t.Error("file outside allow-list accepted")
	}
	if _, err := sc.resolveMediaPath(filepath.Join(sc.StoreDir, "..", filepath.Base(filepath.Dir(outside)), "secret.txt")); err == nil {
		t.Error("traversal out of allow-list accepted")
	}
	if _, err := sc.resolveMediaPath(link); err == nil {
		t.Error("symlink escaping allow-list accepted")
	}
	if _, err := sc.resolveMediaPath(sc.StoreDir); err == nil {
		t.Error("directory accepted as media")
	}
	if _, err := sc.resolveMediaPath(big); err == nil {
		t.Error("oversized file accepted")
	}
	// Must not leak existence of files outside the allow-list.
	e1 := sc.resolveMediaPath2("/etc/passwd")
	e2 := sc.resolveMediaPath2("/etc/definitely-not-here-xyz")
	if e1 != e2 {
		t.Errorf("error message differs for existing vs missing outside file: %q vs %q", e1, e2)
	}
}

func (sc *SecurityConfig) resolveMediaPath2(p string) string {
	_, err := sc.resolveMediaPath(p)
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestProtectMiddleware(t *testing.T) {
	sc := newTestConfig(t)
	called := 0
	h := sc.protect(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})

	do := func(method, host, ctype, auth string) int {
		req := httptest.NewRequest(method, "http://"+host+"/api/send", strings.NewReader(`{}`))
		req.Host = host
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h(rr, req)
		return rr.Code
	}

	good := "Bearer " + sc.Token
	if c := do(http.MethodGet, "127.0.0.1:8080", "application/json", good); c != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d", c)
	}
	if c := do(http.MethodPost, "evil.example.com", "application/json", good); c != http.StatusForbidden {
		t.Errorf("non-loopback Host (DNS rebinding): got %d", c)
	}
	if c := do(http.MethodPost, "127.0.0.1:8080", "text/plain", good); c != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain (CORS simple request): got %d", c)
	}
	if c := do(http.MethodPost, "127.0.0.1:8080", "application/json", ""); c != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d", c)
	}
	if c := do(http.MethodPost, "127.0.0.1:8080", "application/json", "Bearer wrong"); c != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d", c)
	}
	if c := do(http.MethodPost, "127.0.0.1:8080", "application/json", "Bearer "+sc.Token+"x"); c != http.StatusUnauthorized {
		t.Errorf("token with suffix: got %d", c)
	}
	if called != 0 {
		t.Fatalf("handler ran %d times before a valid request", called)
	}
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "localhost"} {
		if c := do(http.MethodPost, host, "application/json; charset=utf-8", good); c != http.StatusOK {
			t.Errorf("valid request with Host %q: got %d", host, c)
		}
	}
	if called != 4 {
		t.Errorf("handler called %d times, want 4", called)
	}
}

func TestDecodeJSONBodyRejectsUnknownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"recipient":"1","message":"m","evil":1}`))
	var s SendMessageRequest
	if err := decodeJSONBody(req, &s); err == nil {
		t.Error("unknown field accepted")
	}
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"recipient":"1","message":"m"}{"recipient":"2"}`))
	if err := decodeJSONBody(req, &s); err == nil {
		t.Error("trailing JSON document accepted")
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	if !l.Allow() || !l.Allow() {
		t.Fatal("first two sends should be allowed")
	}
	if l.Allow() {
		t.Fatal("third send within window should be blocked")
	}
	if !newRateLimiter(0, time.Minute).Allow() {
		t.Fatal("limit 0 should disable limiting")
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	t.Setenv("WHATSAPP_API_TOKEN", "")
	dir := t.TempDir()
	tok1, err := loadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok1) != 64 {
		t.Fatalf("token length %d, want 64 hex chars", len(tok1))
	}
	info, err := os.Stat(filepath.Join(dir, tokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm %o, want 600", perm)
	}
	tok2, err := loadOrCreateToken(dir)
	if err != nil || tok2 != tok1 {
		t.Errorf("token not stable across loads: %q vs %q (%v)", tok1, tok2, err)
	}
	t.Setenv("WHATSAPP_API_TOKEN", "short")
	if _, err := loadOrCreateToken(dir); err == nil {
		t.Error("short env token accepted")
	}
}

func TestAnalyzeOggOpusTruncatedDoesNotPanic(t *testing.T) {
	// A page header claiming more segments/bytes than the buffer holds.
	data := []byte("OggS")
	data = append(data, make([]byte, 22)...)
	data = append(data, 255) // numSegments
	data = append(data, bytes.Repeat([]byte{255}, 255)...)
	data = append(data, []byte("short")...)
	if _, _, err := analyzeOggOpus(data); err != nil {
		t.Logf("analyzeOggOpus returned error (acceptable): %v", err)
	}
	if _, _, err := analyzeOggOpus([]byte("nope")); err == nil {
		t.Error("non-ogg accepted")
	}
}
