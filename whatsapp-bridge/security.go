package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	storeDirName          = "store"
	tokenFileName         = "api_token"
	defaultBindAddr       = "127.0.0.1:8080"
	defaultMaxMediaBytes  = 64 << 20 // 64 MiB
	maxRequestBodyBytes   = 1 << 20  // 1 MiB; requests are small JSON documents
	maxMessageChars       = 65536
	defaultSendsPerMinute = 20
	minTokenLength        = 32
)

// SecurityConfig holds everything the REST API uses to decide whether a
// request is allowed and which files it may touch.
type SecurityConfig struct {
	StoreDir         string   // absolute, symlink-resolved path of the store directory
	Token            string   // bearer token the MCP server must present
	AllowedMediaDirs []string // absolute, symlink-resolved directories files may be sent from
	MaxMediaBytes    int64
	BindAddr         string
	EnforceLoopback  bool // reject requests whose Host header is not a loopback address
	// AllowedRecipients, when non-empty, is the only set of JIDs messages may be sent to.
	AllowedRecipients map[string]bool
	// KnownChatsOnly restricts sending to chats that already exist in the message store.
	KnownChatsOnly bool
	// AuditLogPath receives one JSON line per send attempt; empty disables it.
	AuditLogPath string
	limiter      *rateLimiter
	auditMu      sync.Mutex
}

// rateLimiter is a small sliding-window limiter used to cap outbound sends.
type rateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	events []time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{window: window, max: max}
}

func (r *rateLimiter) Allow() bool {
	if r == nil || r.max <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-r.window)
	kept := r.events[:0]
	for _, t := range r.events {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.events = kept
	if len(r.events) >= r.max {
		return false
	}
	r.events = append(r.events, now)
	return true
}

// ensurePrivateDir creates a directory readable only by the current user.
func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// tightenFilePerms makes a regular file readable only by the current user.
func tightenFilePerms(path string) {
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		_ = os.Chmod(path, 0o600)
	}
}

// loadOrCreateToken returns the API bearer token from WHATSAPP_API_TOKEN or
// store/api_token, generating the file on first run.
func loadOrCreateToken(storeDir string) (string, error) {
	if t := strings.TrimSpace(os.Getenv("WHATSAPP_API_TOKEN")); t != "" {
		if len(t) < minTokenLength {
			return "", fmt.Errorf("WHATSAPP_API_TOKEN must be at least %d characters", minTokenLength)
		}
		return t, nil
	}

	path := filepath.Join(storeDir, tokenFileName)
	if data, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(data))
		if len(t) >= minTokenLength {
			tightenFilePerms(path)
			return t, nil
		}
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate API token: %w", err)
	}
	t := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("failed to write API token: %w", err)
	}
	tightenFilePerms(path)
	return t, nil
}

func resolveDir(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	return real, nil
}

// loadAllowedMediaDirs returns the store directory plus WHATSAPP_MEDIA_DIRS.
func loadAllowedMediaDirs(storeDir string, logger waLog.Logger) []string {
	dirs := []string{storeDir}
	for _, d := range filepath.SplitList(os.Getenv("WHATSAPP_MEDIA_DIRS")) {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		real, err := resolveDir(d)
		if err != nil {
			logger.Warnf("Ignoring WHATSAPP_MEDIA_DIRS entry %q: %v", d, err)
			continue
		}
		dirs = append(dirs, real)
	}
	return dirs
}

// isWithinDir reports whether path is dir itself or lies underneath it.
func isWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

func (sc *SecurityConfig) inAllowedMediaDir(path string) bool {
	for _, d := range sc.AllowedMediaDirs {
		if isWithinDir(path, d) {
			return true
		}
	}
	return false
}

// resolveMediaPath validates a caller-supplied path for sending. The lexical
// check runs before any filesystem access so the API cannot be used as a
// file-existence oracle for paths outside the allow-list.
func (sc *SecurityConfig) resolveMediaPath(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", errors.New("media_path must be an absolute path")
	}
	clean := filepath.Clean(p)
	if !sc.inAllowedMediaDir(clean) {
		return "", errors.New("media_path is outside the allowed media directories (configure WHATSAPP_MEDIA_DIRS on the bridge)")
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", errors.New("media file not found")
	}
	if !sc.inAllowedMediaDir(real) {
		return "", errors.New("media_path resolves outside the allowed media directories")
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("media_path is not a regular file")
	}
	if info.Size() > sc.MaxMediaBytes {
		return "", fmt.Errorf("media file exceeds the %d byte limit", sc.MaxMediaBytes)
	}
	return real, nil
}

var (
	unsafeFilenameChars = regexp.MustCompile(`[^\p{L}\p{N}._ ()\-]`)
	unsafePathComponent = regexp.MustCompile(`[^A-Za-z0-9@._\-]`)
	phoneNumberPattern  = regexp.MustCompile(`^[0-9]{5,20}$`)
)

// sanitizeFilename reduces an untrusted filename to a single safe component.
func sanitizeFilename(name, fallback string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	if name == "" || name == "." || name == ".." {
		name = fallback
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

// safePathComponent turns a chat JID or message ID into a directory/file name.
func safePathComponent(s string) string {
	s = unsafePathComponent.ReplaceAllString(s, "_")
	s = strings.Trim(s, ".")
	if s == "" {
		return "_"
	}
	return s
}

// parseRecipient accepts a bare phone number (digits only, with country
// code) or a JID on one of the servers we are willing to send to.
func parseRecipient(recipient string) (types.JID, error) {
	recipient = strings.TrimSpace(recipient)
	if strings.Contains(recipient, "@") {
		jid, err := types.ParseJID(recipient)
		if err != nil {
			return types.JID{}, fmt.Errorf("invalid JID: %v", err)
		}
		switch jid.Server {
		case types.DefaultUserServer, types.GroupServer, types.HiddenUserServer, types.NewsletterServer:
		default:
			return types.JID{}, fmt.Errorf("unsupported JID server %q", jid.Server)
		}
		if jid.User == "" {
			return types.JID{}, errors.New("JID has no user part")
		}
		return jid, nil
	}
	if !phoneNumberPattern.MatchString(recipient) {
		return types.JID{}, errors.New("recipient must be a phone number (digits only, with country code) or a JID")
	}
	return types.NewJID(recipient, types.DefaultUserServer), nil
}

// parseRecipientList parses WHATSAPP_ALLOWED_RECIPIENTS.
func parseRecipientList(raw string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		jid, err := parseRecipient(item)
		if err != nil {
			return nil, fmt.Errorf("WHATSAPP_ALLOWED_RECIPIENTS entry %q: %w", item, err)
		}
		out[jid.ToNonAD().String()] = true
	}
	return out, nil
}

// recipientAllowed applies the optional recipient allow-list and the
// known-chats-only rule. isKnownChat is injected so the rule can be tested
// without a live store.
func (sc *SecurityConfig) recipientAllowed(jid types.JID, isKnownChat func(string) bool) error {
	key := jid.ToNonAD().String()
	if len(sc.AllowedRecipients) > 0 && !sc.AllowedRecipients[key] {
		return fmt.Errorf("recipient %s is not in WHATSAPP_ALLOWED_RECIPIENTS", key)
	}
	if sc.KnownChatsOnly && (isKnownChat == nil || !isKnownChat(key)) {
		return fmt.Errorf("recipient %s is not an existing chat and WHATSAPP_SEND_KNOWN_CHATS_ONLY is set", key)
	}
	return nil
}

type auditEntry struct {
	Time         string `json:"time"`
	Recipient    string `json:"recipient"`
	Kind         string `json:"kind"`
	Media        string `json:"media,omitempty"`
	MessageChars int    `json:"message_chars"`
	Success      bool   `json:"success"`
	Result       string `json:"result"`
}

// audit appends one JSON line per send attempt. Message bodies are never written.
func (sc *SecurityConfig) audit(e auditEntry) {
	if sc.AuditLogPath == "" {
		return
	}
	e.Time = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	sc.auditMu.Lock()
	defer sc.auditMu.Unlock()
	f, err := os.OpenFile(sc.AuditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func isLoopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isJSONContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "application/json"
}

func (sc *SecurityConfig) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	presented := strings.TrimSpace(auth[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(presented), []byte(sc.Token)) == 1
}

// protect wraps an API handler with the checks every request must pass:
// POST only, loopback Host (blocks DNS rebinding), JSON content type and a
// non-simple Authorization header (both block cross-site requests from
// browsers), bearer token, and a bounded request body.
func (sc *SecurityConfig) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sc.EnforceLoopback && !isLoopbackHost(r.Host) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		if !sc.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next(w, r)
	}
}

func decodeJSONBody(r *http.Request, dst interface{}) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after JSON body")
	}
	return nil
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func loadSecurityConfig(logger waLog.Logger) (*SecurityConfig, error) {
	if err := ensurePrivateDir(storeDirName); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %w", err)
	}
	storeDir, err := resolveDir(storeDirName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve store directory: %w", err)
	}

	token, err := loadOrCreateToken(storeDir)
	if err != nil {
		return nil, err
	}

	bindAddr := strings.TrimSpace(os.Getenv("WHATSAPP_BRIDGE_ADDR"))
	if bindAddr == "" {
		bindAddr = defaultBindAddr
	}
	bindHost, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return nil, fmt.Errorf("WHATSAPP_BRIDGE_ADDR must be host:port: %w", err)
	}
	enforceLoopback := bindHost == "" || isLoopbackHost(bindHost)
	if !enforceLoopback {
		logger.Warnf("WHATSAPP_BRIDGE_ADDR=%s is not a loopback address. The API will be reachable from other machines; anyone with the token can read your messages and send as you.", bindAddr)
	}

	maxMedia := int64(envInt("WHATSAPP_MAX_MEDIA_BYTES", defaultMaxMediaBytes))
	if maxMedia <= 0 {
		maxMedia = defaultMaxMediaBytes
	}

	sc := &SecurityConfig{
		StoreDir:         storeDir,
		Token:            token,
		AllowedMediaDirs: loadAllowedMediaDirs(storeDir, logger),
		MaxMediaBytes:    maxMedia,
		BindAddr:         bindAddr,
		EnforceLoopback:  enforceLoopback,
		limiter:          newRateLimiter(envInt("WHATSAPP_SEND_RATE_LIMIT", defaultSendsPerMinute), time.Minute),
	}

	sc.AllowedRecipients, err = parseRecipientList(os.Getenv("WHATSAPP_ALLOWED_RECIPIENTS"))
	if err != nil {
		return nil, err
	}
	sc.KnownChatsOnly = envBool("WHATSAPP_SEND_KNOWN_CHATS_ONLY", false)
	if !envBool("WHATSAPP_DISABLE_AUDIT_LOG", false) {
		sc.AuditLogPath = filepath.Join(storeDir, "send_audit.log")
	}

	logger.Infof("Media may be sent from: %s", strings.Join(sc.AllowedMediaDirs, ", "))
	if len(sc.AllowedRecipients) > 0 {
		logger.Infof("Sending restricted to %d allow-listed recipient(s)", len(sc.AllowedRecipients))
	}
	if sc.KnownChatsOnly {
		logger.Infof("Sending restricted to existing chats")
	}
	return sc, nil
}
