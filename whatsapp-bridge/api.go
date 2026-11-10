package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// apiServer is the authenticated loopback REST API the MCP server talks to.
// Every read goes through here too, so the MCP process never touches the
// bridge's data directly.
type apiServer struct {
	client *whatsmeow.Client
	store  *MemoryStore
	sc     *SecurityConfig
	hist   *historyRequester
	logger waLog.Logger
	// contacts returns the phone's contact list; nil disables that source.
	contacts func(ctx context.Context) (map[types.JID]types.ContactInfo, error)
}

const (
	maxLimit        = 100
	maxPage         = 10000
	maxContext      = 50
	maxHistoryCount = 200
	maxQueryLength  = 500
)

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseTime accepts RFC3339, "2006-01-02 15:04:05" and "2006-01-02".
func parseTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid timestamp %q; use ISO-8601", s)
}

type apiError struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Success: false, Message: msg})
}

// --- request/response types -------------------------------------------------

type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

type listChatsRequest struct {
	Query              string `json:"query"`
	Limit              int    `json:"limit"`
	Page               int    `json:"page"`
	SortBy             string `json:"sort_by"`
	IncludeLastMessage *bool  `json:"include_last_message"`
	DirectOnly         bool   `json:"direct_only"`
}

type getChatRequest struct {
	ChatJID            string `json:"chat_jid"`
	IncludeLastMessage *bool  `json:"include_last_message"`
}

type listMessagesRequest struct {
	ChatJID        string `json:"chat_jid"`
	Sender         string `json:"sender"`
	Query          string `json:"query"`
	After          string `json:"after"`
	Before         string `json:"before"`
	Limit          int    `json:"limit"`
	Page           int    `json:"page"`
	IncludeContext bool   `json:"include_context"`
	ContextBefore  int    `json:"context_before"`
	ContextAfter   int    `json:"context_after"`
}

type messageContextRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
}

type contactsRequest struct {
	Query string `json:"query"`
}

type contactChatsRequest struct {
	JID   string `json:"jid"`
	Limit int    `json:"limit"`
	Page  int    `json:"page"`
}

type lastInteractionRequest struct {
	JID string `json:"jid"`
}

type historyRequest struct {
	ChatJID string `json:"chat_jid"`
	Count   int    `json:"count"`
}

// --- handlers ---------------------------------------------------------------

func (a *apiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	var req SendMessageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.Recipient == "" {
		writeErr(w, http.StatusBadRequest, "Recipient is required")
		return
	}
	if req.Message == "" && req.MediaPath == "" {
		writeErr(w, http.StatusBadRequest, "Message or media path is required")
		return
	}
	if len(req.Message) > maxMessageChars {
		writeErr(w, http.StatusBadRequest, "Message is too long")
		return
	}
	if !a.sc.limiter.Allow() {
		writeJSON(w, http.StatusTooManyRequests, SendMessageResponse{Success: false, Message: "Send rate limit exceeded; try again shortly"})
		return
	}

	kind := "text"
	if req.MediaPath != "" {
		kind = "media"
	}
	a.logger.Infof("Send request: recipient=%s kind=%s", req.Recipient, kind)

	success, message := sendWhatsAppMessage(a.client, a.store, a.sc, req.Recipient, req.Message, req.MediaPath)

	media := ""
	if req.MediaPath != "" {
		media = filepath.Base(req.MediaPath)
	}
	a.sc.audit(auditEntry{Recipient: req.Recipient, Kind: kind, Media: media, MessageChars: len(req.Message), Success: success, Result: message})

	status := http.StatusOK
	if !success {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, SendMessageResponse{Success: success, Message: message})
}

func (a *apiServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req DownloadMediaRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.MessageID == "" || req.ChatJID == "" {
		writeErr(w, http.StatusBadRequest, "Message ID and Chat JID are required")
		return
	}
	success, mediaType, filename, path, err := downloadMedia(a.client, a.store, a.sc, req.MessageID, req.ChatJID)
	if !success || err != nil {
		msg := "Unknown error"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, http.StatusInternalServerError, DownloadMediaResponse{Success: false, Message: "Failed to download media: " + msg})
		return
	}
	writeJSON(w, http.StatusOK, DownloadMediaResponse{Success: true, Message: fmt.Sprintf("Successfully downloaded %s media", mediaType), Filename: filename, Path: path})
}

func (a *apiServer) handleChats(w http.ResponseWriter, r *http.Request) {
	var req listChatsRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	limit := clamp(req.Limit, 1, maxLimit, 20)
	page := clamp(req.Page, 0, maxPage, 0)
	includeLast := req.IncludeLastMessage == nil || *req.IncludeLastMessage
	chats := a.store.ListChats(truncate(req.Query, maxQueryLength), limit, page*limit, req.SortBy == "name", includeLast, req.DirectOnly)
	if chats == nil {
		chats = []ChatView{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "chats": chats})
}

func (a *apiServer) handleChat(w http.ResponseWriter, r *http.Request) {
	var req getChatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.ChatJID == "" {
		writeErr(w, http.StatusBadRequest, "chat_jid is required")
		return
	}
	includeLast := req.IncludeLastMessage == nil || *req.IncludeLastMessage
	chat, ok := a.store.GetChat(req.ChatJID, includeLast)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "chat": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "chat": chat})
}

func (a *apiServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req listMessagesRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	after, err := parseTime(req.After)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := parseTime(req.Before)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clamp(req.Limit, 1, maxLimit, 20)
	page := clamp(req.Page, 0, maxPage, 0)

	hits := a.store.ListMessages(MessageFilter{
		ChatJID: req.ChatJID,
		Sender:  req.Sender,
		Query:   truncate(req.Query, maxQueryLength),
		After:   after,
		Before:  before,
		Limit:   limit,
		Offset:  page * limit,
	})

	if req.IncludeContext && len(hits) > 0 {
		cb := clamp(req.ContextBefore, 0, maxContext, 1)
		ca := clamp(req.ContextAfter, 0, maxContext, 1)
		seen := map[string]bool{}
		var expanded []*StoredMessage
		add := func(m *StoredMessage) {
			key := m.ChatJID + "/" + m.ID
			if !seen[key] {
				seen[key] = true
				expanded = append(expanded, m)
			}
		}
		for _, hit := range hits {
			target, beforeMsgs, afterMsgs, ok := a.store.MessageContext(hit.ID, hit.ChatJID, cb, ca)
			if !ok {
				add(hit)
				continue
			}
			for _, m := range beforeMsgs {
				add(m)
			}
			add(target)
			for _, m := range afterMsgs {
				add(m)
			}
		}
		hits = expanded
	}
	if hits == nil {
		hits = []*StoredMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "messages": hits, "cached_messages": a.store.Count()})
}

func (a *apiServer) handleMessageContext(w http.ResponseWriter, r *http.Request) {
	var req messageContextRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.MessageID == "" {
		writeErr(w, http.StatusBadRequest, "message_id is required")
		return
	}
	target, before, after, ok := a.store.MessageContext(req.MessageID, req.ChatJID, clamp(req.Before, 0, maxContext, 5), clamp(req.After, 0, maxContext, 5))
	if !ok {
		writeErr(w, http.StatusNotFound, "Message not found in the cache")
		return
	}
	if before == nil {
		before = []*StoredMessage{}
	}
	if after == nil {
		after = []*StoredMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": target, "before": before, "after": after})
}

func (a *apiServer) handleContacts(w http.ResponseWriter, r *http.Request) {
	var req contactsRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	query := strings.ToLower(truncate(strings.TrimSpace(req.Query), maxQueryLength))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "contacts": []Contact{}})
		return
	}

	byJID := map[string]Contact{}
	for _, c := range a.store.SearchContacts(query, 50) {
		byJID[c.JID] = c
	}
	if a.contacts != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if all, err := a.contacts(ctx); err == nil {
			for jid, info := range all {
				if jid.Server != types.DefaultUserServer {
					continue
				}
				name := info.FullName
				if name == "" {
					name = info.PushName
				}
				if name == "" {
					name = info.BusinessName
				}
				if !strings.Contains(strings.ToLower(name), query) && !strings.Contains(jid.User, query) {
					continue
				}
				key := jid.ToNonAD().String()
				if existing, ok := byJID[key]; ok && existing.Name != "" {
					continue
				}
				byJID[key] = Contact{JID: key, PhoneNumber: jid.User, Name: name}
			}
		} else {
			a.logger.Warnf("Contact lookup failed: %v", err)
		}
	}
	out := make([]Contact, 0, len(byJID))
	for _, c := range byJID {
		out = append(out, c)
	}
	sortContacts(out)
	if len(out) > 50 {
		out = out[:50]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "contacts": out})
}

func (a *apiServer) handleContactChats(w http.ResponseWriter, r *http.Request) {
	var req contactChatsRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.JID == "" {
		writeErr(w, http.StatusBadRequest, "jid is required")
		return
	}
	limit := clamp(req.Limit, 1, maxLimit, 20)
	page := clamp(req.Page, 0, maxPage, 0)
	chats := a.store.ChatsInvolving(req.JID, limit, page*limit)
	if chats == nil {
		chats = []ChatView{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "chats": chats})
}

func (a *apiServer) handleLastInteraction(w http.ResponseWriter, r *http.Request) {
	var req lastInteractionRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.JID == "" {
		writeErr(w, http.StatusBadRequest, "jid is required")
		return
	}
	msg, ok := a.store.LastInteraction(req.JID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": msg})
}

func (a *apiServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	var req historyRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.ChatJID == "" {
		writeErr(w, http.StatusBadRequest, "chat_jid is required")
		return
	}
	count := clamp(req.Count, 1, maxHistoryCount, 50)
	a.logger.Infof("History request: chat=%s count=%d", req.ChatJID, count)
	added, err := a.hist.FetchOlder(r.Context(), a.client, a.store, req.ChatJID, count)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			status = http.StatusRequestTimeout
		}
		writeJSON(w, status, map[string]interface{}{"success": false, "added": 0, "message": err.Error()})
		return
	}
	msg := fmt.Sprintf("Phone delivered %d older message(s) for %s", added, req.ChatJID)
	if added == 0 {
		msg = "The phone returned no older messages; you may have reached the beginning of the chat"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "added": added, "cached_messages": a.store.Count(), "message": msg})
}

func sortContacts(cs []Contact) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && (cs[j].Name < cs[j-1].Name || (cs[j].Name == cs[j-1].Name && cs[j].JID < cs[j-1].JID)); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// routes wires every endpoint behind the security middleware.
func (a *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	p := a.sc.protect
	mux.HandleFunc("/api/send", p(a.handleSend))
	mux.HandleFunc("/api/download", p(a.handleDownload))
	mux.HandleFunc("/api/chats", p(a.handleChats))
	mux.HandleFunc("/api/chat", p(a.handleChat))
	mux.HandleFunc("/api/messages", p(a.handleMessages))
	mux.HandleFunc("/api/message_context", p(a.handleMessageContext))
	mux.HandleFunc("/api/contacts", p(a.handleContacts))
	mux.HandleFunc("/api/contact_chats", p(a.handleContactChats))
	mux.HandleFunc("/api/last_interaction", p(a.handleLastInteraction))
	mux.HandleFunc("/api/history", p(a.handleHistory))
	// Liveness probe for containers. Unauthenticated on purpose and limited
	// to connection state; it exposes no account data.
	mux.HandleFunc("/health", a.handleHealth)
	return mux
}

func (a *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	connected, loggedIn := false, false
	if a.client != nil {
		connected = a.client.IsConnected()
		loggedIn = a.client.IsLoggedIn()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": connected, "logged_in": loggedIn})
}

// start runs the HTTP server in the background and returns it for shutdown.
func (a *apiServer) start() *http.Server {
	srv := &http.Server{
		Addr:              a.sc.BindAddr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute, // media transfers and phone history requests can be slow
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	fmt.Printf("Starting REST API server on %s (bearer token in %s)...\n", a.sc.BindAddr, filepath.Join(a.sc.StoreDir, tokenFileName))
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
	return srv
}
