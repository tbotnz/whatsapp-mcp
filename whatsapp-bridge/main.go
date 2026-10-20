package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Security configuration
// ---------------------------------------------------------------------------

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
	limiter          *rateLimiter
}

// rateLimiter is a small sliding-window limiter used to cap outbound sends.
// It bounds the damage an over-eager or prompt-injected agent can do.
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

// loadOrCreateToken returns the API bearer token. It is taken from the
// WHATSAPP_API_TOKEN environment variable if set, otherwise from
// store/api_token, which is generated on first run.
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

// loadAllowedMediaDirs returns the directories from which media may be sent:
// always the store directory, plus any entries in WHATSAPP_MEDIA_DIRS
// (separated by the OS path-list separator, ':' on Unix).
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
// Both arguments must be absolute and cleaned.
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

// sanitizeFilename reduces an untrusted filename (for example one chosen by
// the sender of a WhatsApp document) to a single safe path component.
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

// safePathComponent turns a chat JID into a directory name.
func safePathComponent(s string) string {
	s = unsafePathComponent.ReplaceAllString(s, "_")
	s = strings.Trim(s, ".")
	if s == "" {
		return "_"
	}
	return s
}

// parseRecipient accepts either a bare phone number (digits only, with
// country code) or a JID on one of the servers we are willing to send to.
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
	// Reject trailing garbage / multiple documents.
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
		logger.Warnf("WHATSAPP_BRIDGE_ADDR=%s is not a loopback address. The API will be reachable from other machines; anyone with the token can send messages and read media as you.", bindAddr)
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
	logger.Infof("Media may be sent from: %s", strings.Join(sc.AllowedMediaDirs, ", "))
	return sc, nil
}

// ---------------------------------------------------------------------------
// Message store
// ---------------------------------------------------------------------------

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	if err := ensurePrivateDir(storeDirName); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	dbPath := filepath.Join(storeDirName, "messages.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}
	tightenFilePerms(dbPath)

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, sc *SecurityConfig, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	recipientJID, err := parseRecipient(recipient)
	if err != nil {
		return false, err.Error()
	}

	msg := &waProto.Message{}

	if mediaPath != "" {
		resolvedPath, err := sc.resolveMediaPath(mediaPath)
		if err != nil {
			return false, err.Error()
		}

		mediaData, err := os.ReadFile(resolvedPath)
		if err != nil {
			return false, "Error reading media file"
		}

		fileExt := strings.ToLower(strings.TrimPrefix(filepath.Ext(resolvedPath), "."))
		var mediaType whatsmeow.MediaType
		var mimeType string

		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(filepath.Base(resolvedPath)),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	_, err = client.SendMessage(context.Background(), recipientJID, msg)
	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipientJID.String())
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	if doc := msg.GetDocumentMessage(); doc != nil {
		// The filename is chosen by the remote sender: sanitize before storing.
		fallback := "document_" + time.Now().Format("20060102_150405")
		filename := sanitizeFilename(doc.GetFileName(), fallback)
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	content := extractTextContent(msg.Message)
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	if content == "" && mediaType == "" {
		return
	}

	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
		return
	}

	// Log metadata only; message bodies are private and stay in the database.
	direction := "←"
	if msg.Info.IsFromMe {
		direction = "→"
	}
	kind := "text"
	if mediaType != "" {
		kind = mediaType
	}
	fmt.Printf("[%s] %s %s (%s)\n", msg.Info.Timestamp.Format("2006-01-02 15:04:05"), direction, chatJID, kind)
}

// ---------------------------------------------------------------------------
// Media download
// ---------------------------------------------------------------------------

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

func (d *MediaDownloader) GetDirectPath() string             { return d.DirectPath }
func (d *MediaDownloader) GetURL() string                    { return d.URL }
func (d *MediaDownloader) GetMediaKey() []byte               { return d.MediaKey }
func (d *MediaDownloader) GetFileLength() uint64             { return d.FileLength }
func (d *MediaDownloader) GetFileSHA256() []byte             { return d.FileSHA256 }
func (d *MediaDownloader) GetFileEncSHA256() []byte          { return d.FileEncSHA256 }
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType { return d.MediaType }

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, sc *SecurityConfig, messageID, chatJID string) (bool, string, string, string, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)
	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Build the local path from sanitized components and verify it stays
	// inside the store directory. Both the chat JID and the filename
	// originate from the network.
	chatDir := filepath.Join(sc.StoreDir, safePathComponent(chatJID))
	fallback := mediaType + "_" + safePathComponent(messageID)
	filename = sanitizeFilename(filename, fallback)
	localPath := filepath.Join(chatDir, filename)
	if !isWithinDir(localPath, sc.StoreDir) || !isWithinDir(chatDir, sc.StoreDir) {
		return false, "", "", "", fmt.Errorf("refusing to write outside the store directory")
	}

	if err := ensurePrivateDir(chatDir); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Check if file already exists
	if info, err := os.Lstat(localPath); err == nil {
		if !info.Mode().IsRegular() {
			return false, "", "", "", fmt.Errorf("existing media path is not a regular file")
		}
		return true, mediaType, filename, localPath, nil
	}

	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	directPath := extractDirectPathFromURL(url)

	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	mediaData, err := client.Download(ctx, downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	if err := os.WriteFile(localPath, mediaData, 0o600); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Downloaded %s media for message %s (%d bytes)\n", mediaType, messageID, len(mediaData))
	return true, mediaType, filename, localPath, nil
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url
	}

	pathPart := parts[1]
	pathPart = strings.SplitN(pathPart, "?", 2)[0]
	return "/" + pathPart
}

// ---------------------------------------------------------------------------
// REST API
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, sc *SecurityConfig, logger waLog.Logger) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/send", sc.protect(func(w http.ResponseWriter, r *http.Request) {
		var req SendMessageRequest
		if err := decodeJSONBody(r, &req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}
		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}
		if len(req.Message) > maxMessageChars {
			http.Error(w, "Message is too long", http.StatusBadRequest)
			return
		}

		if !sc.limiter.Allow() {
			writeJSON(w, http.StatusTooManyRequests, SendMessageResponse{
				Success: false,
				Message: "Send rate limit exceeded; try again shortly",
			})
			return
		}

		kind := "text"
		if req.MediaPath != "" {
			kind = "media"
		}
		logger.Infof("Send request: recipient=%s kind=%s", req.Recipient, kind)

		success, message := sendWhatsAppMessage(client, sc, req.Recipient, req.Message, req.MediaPath)

		status := http.StatusOK
		if !success {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, SendMessageResponse{Success: success, Message: message})
	}))

	mux.HandleFunc("/api/download", sc.protect(func(w http.ResponseWriter, r *http.Request) {
		var req DownloadMediaRequest
		if err := decodeJSONBody(r, &req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		success, mediaType, filename, path, err := downloadMedia(client, messageStore, sc, req.MessageID, req.ChatJID)
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}
			writeJSON(w, http.StatusInternalServerError, DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		writeJSON(w, http.StatusOK, DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	}))

	srv := &http.Server{
		Addr:              sc.BindAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute, // media uploads/downloads can be slow
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	fmt.Printf("Starting REST API server on %s (bearer token in %s)...\n", sc.BindAddr, filepath.Join(sc.StoreDir, tokenFileName))

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()

	return srv
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	dbLog := waLog.Stdout("Database", "INFO", true)

	sc, err := loadSecurityConfig(logger)
	if err != nil {
		logger.Errorf("%v", err)
		return
	}

	ctx := context.Background()
	sessionDBPath := filepath.Join(storeDirName, "whatsapp.db")
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+sessionDBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}
	// The session database holds the Signal keys for this device; anyone who
	// can read it can act as this WhatsApp account.
	tightenFilePerms(sessionDBPath)

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	connected := make(chan bool, 1)

	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(ctx)
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	// Pairing may have just created/modified the session database.
	tightenFilePerms(sessionDBPath)

	fmt.Println("\n✓ Connected to WhatsApp!")

	srv := startRESTServer(client, messageStore, sc, logger)

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	<-exitChan

	fmt.Println("Disconnecting...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		return existingName
	}

	var name string

	if jid.Server == types.GroupServer {
		if conversation != nil {
			// Extract name from conversation if available
			var displayName, convName *string
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}
	} else {
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			name = sender
		} else {
			name = jid.User
		}
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		messages := conversation.Messages
		if len(messages) > 0 {
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				if content == "" && mediaType == "" {
					continue
				}

				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	for i := 0; i < len(data); {
		if i+27 >= len(data) {
			break
		}

		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}
		if i+pageSize > len(data) {
			// Truncated page; stop rather than slice past the buffer.
			break
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8
				if headPos+16 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
				}
			}
		}

		if granulePos != 0 {
			lastGranule = granulePos
		}

		i += pageSize
	}

	if sampleRate == 0 {
		sampleRate = 48000
	}

	if lastGranule > 0 {
		var samples uint64
		if lastGranule > uint64(preSkip) {
			samples = lastGranule - uint64(preSkip)
		}
		durationSeconds := float64(samples) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
	} else {
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	waveform = placeholderWaveform(duration)

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Deterministic for a given duration; this is cosmetic, not security-relevant.
	rng := mathrand.New(mathrand.NewSource(int64(duration)))

	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		pos := float64(i) / float64(waveformLength)

		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)
		val += (rng.Float64() - 0.5) * 15

		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)
		val = val + 50

		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
