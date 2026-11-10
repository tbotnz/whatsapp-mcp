package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The bridge keeps messages in memory only. Nothing a message contains
// (text, media keys, filenames) is ever written to disk by this file. The one
// optional on-disk artifact is the chat index: chat JIDs, display names,
// timestamps and message IDs, which lets the bridge ask the phone for older
// history after a restart. It can be disabled with WHATSAPP_PERSIST_CHAT_INDEX=0.

// MessageAnchor identifies a message well enough to ask the phone for the
// messages around it. It carries no content.
type MessageAnchor struct {
	ID        string    `json:"id"`
	IsFromMe  bool      `json:"is_from_me"`
	Timestamp time.Time `json:"timestamp"`
}

// Chat is the per-conversation record.
type Chat struct {
	JID             string         `json:"jid"`
	Name            string         `json:"name"`
	LastMessageTime time.Time      `json:"last_message_time"`
	Oldest          *MessageAnchor `json:"oldest,omitempty"`
	Newest          *MessageAnchor `json:"newest,omitempty"`
}

// mediaInfo holds what is needed to download a message's attachment later.
// It never leaves process memory.
type mediaInfo struct {
	URL           string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// StoredMessage is one cached message. The JSON form is what the REST API
// returns; the media field is deliberately unexported.
type StoredMessage struct {
	ID         string    `json:"id"`
	ChatJID    string    `json:"chat_jid"`
	ChatName   string    `json:"chat_name,omitempty"`
	Sender     string    `json:"sender"`
	SenderName string    `json:"sender_name,omitempty"`
	Content    string    `json:"content"`
	Timestamp  time.Time `json:"timestamp"`
	IsFromMe   bool      `json:"is_from_me"`
	MediaType  string    `json:"media_type,omitempty"`
	Filename   string    `json:"filename,omitempty"`
	media      *mediaInfo
}

// ChatView is the API shape of a chat, with its latest message folded in.
type ChatView struct {
	JID             string     `json:"jid"`
	Name            string     `json:"name"`
	LastMessageTime *time.Time `json:"last_message_time"`
	LastMessage     *string    `json:"last_message"`
	LastSender      *string    `json:"last_sender"`
	LastIsFromMe    *bool      `json:"last_is_from_me"`
	IsGroup         bool       `json:"is_group"`
}

// MessageFilter selects messages for ListMessages.
type MessageFilter struct {
	ChatJID string
	Sender  string
	Query   string
	After   *time.Time
	Before  *time.Time
	Limit   int
	Offset  int
}

// MemoryStore is a bounded, thread-safe in-memory message cache.
type MemoryStore struct {
	mu          sync.RWMutex
	chats       map[string]*Chat
	messages    map[string]map[string]*StoredMessage // chat JID -> message ID -> message
	count       int
	maxMessages int
	indexPath   string
	dirty       bool
}

func NewMemoryStore(maxMessages int, indexPath string) *MemoryStore {
	if maxMessages < 100 {
		maxMessages = 100
	}
	return &MemoryStore{
		chats:       map[string]*Chat{},
		messages:    map[string]map[string]*StoredMessage{},
		maxMessages: maxMessages,
		indexPath:   indexPath,
	}
}

// Count returns the number of cached messages.
func (s *MemoryStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count
}

func (s *MemoryStore) chatLocked(jid string) *Chat {
	c := s.chats[jid]
	if c == nil {
		c = &Chat{JID: jid}
		s.chats[jid] = c
	}
	return c
}

// UpsertChat records a chat. An empty name leaves any existing name alone.
func (s *MemoryStore) UpsertChat(jid, name string, lastMessageTime time.Time) {
	if jid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chatLocked(jid)
	if name != "" {
		c.Name = name
	}
	if lastMessageTime.After(c.LastMessageTime) {
		c.LastMessageTime = lastMessageTime
	}
	s.dirty = true
}

// ChatName returns the stored display name for a chat, or "".
func (s *MemoryStore) ChatName(jid string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c := s.chats[jid]; c != nil {
		return c.Name
	}
	return ""
}

// HasChat reports whether the chat is known (from the cache or the index).
func (s *MemoryStore) HasChat(jid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.chats[jid]
	return ok
}

// AddMessage caches a message and returns true if it was not already present.
// Messages with neither text nor media are ignored.
func (s *MemoryStore) AddMessage(m *StoredMessage) bool {
	if m == nil || m.ID == "" || m.ChatJID == "" || (m.Content == "" && m.MediaType == "") {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	byID := s.messages[m.ChatJID]
	if byID == nil {
		byID = map[string]*StoredMessage{}
		s.messages[m.ChatJID] = byID
	}
	_, exists := byID[m.ID]
	byID[m.ID] = m

	c := s.chatLocked(m.ChatJID)
	if m.Timestamp.After(c.LastMessageTime) {
		c.LastMessageTime = m.Timestamp
	}
	anchor := &MessageAnchor{ID: m.ID, IsFromMe: m.IsFromMe, Timestamp: m.Timestamp}
	if c.Oldest == nil || m.Timestamp.Before(c.Oldest.Timestamp) {
		c.Oldest = anchor
	}
	if c.Newest == nil || m.Timestamp.After(c.Newest.Timestamp) {
		c.Newest = anchor
	}

	if !exists {
		s.count++
		s.evictLocked()
	}
	s.dirty = true
	return !exists
}

// evictLocked drops the oldest messages when the cache is over its cap. It
// removes a batch at a time so the sort cost is amortised. Chat anchors are
// kept, so older history can still be requested from the phone.
func (s *MemoryStore) evictLocked() {
	if s.count <= s.maxMessages {
		return
	}
	target := s.maxMessages - s.maxMessages/20 // free 5% headroom
	all := make([]*StoredMessage, 0, s.count)
	for _, byID := range s.messages {
		for _, m := range byID {
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp.Before(all[j].Timestamp) })
	for _, m := range all {
		if s.count <= target {
			break
		}
		delete(s.messages[m.ChatJID], m.ID)
		if len(s.messages[m.ChatJID]) == 0 {
			delete(s.messages, m.ChatJID)
		}
		s.count--
	}
}

// senderNameLocked maps a sender (phone number or JID user part) to the name
// of their direct chat, if we have one.
func (s *MemoryStore) senderNameLocked(sender string) string {
	if sender == "" {
		return ""
	}
	user := sender
	if i := strings.IndexByte(user, '@'); i >= 0 {
		user = user[:i]
	}
	if c := s.chats[user+"@s.whatsapp.net"]; c != nil && c.Name != "" {
		return c.Name
	}
	if c := s.chats[user+"@lid"]; c != nil && c.Name != "" {
		return c.Name
	}
	return ""
}

func (s *MemoryStore) decorateLocked(m *StoredMessage) *StoredMessage {
	out := *m
	out.media = nil
	if c := s.chats[m.ChatJID]; c != nil {
		out.ChatName = c.Name
	}
	if m.IsFromMe {
		out.SenderName = "Me"
	} else if n := s.senderNameLocked(m.Sender); n != "" {
		out.SenderName = n
	} else {
		out.SenderName = m.Sender
	}
	return &out
}

func sortDesc(ms []*StoredMessage) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Timestamp.Equal(ms[j].Timestamp) {
			return ms[i].ID > ms[j].ID
		}
		return ms[i].Timestamp.After(ms[j].Timestamp)
	})
}

func sortAsc(ms []*StoredMessage) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Timestamp.Equal(ms[j].Timestamp) {
			return ms[i].ID < ms[j].ID
		}
		return ms[i].Timestamp.Before(ms[j].Timestamp)
	})
}

func senderMatches(sender, want string) bool {
	if want == "" {
		return true
	}
	if sender == want {
		return true
	}
	if i := strings.IndexByte(want, '@'); i >= 0 {
		return sender == want[:i]
	}
	return false
}

// ListMessages returns messages matching the filter, newest first.
func (s *MemoryStore) ListMessages(f MessageFilter) []*StoredMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := strings.ToLower(f.Query)
	var hits []*StoredMessage
	consider := func(byID map[string]*StoredMessage) {
		for _, m := range byID {
			if !senderMatches(m.Sender, f.Sender) {
				continue
			}
			if f.After != nil && !m.Timestamp.After(*f.After) {
				continue
			}
			if f.Before != nil && !m.Timestamp.Before(*f.Before) {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(m.Content), query) {
				continue
			}
			hits = append(hits, m)
		}
	}
	if f.ChatJID != "" {
		consider(s.messages[f.ChatJID])
	} else {
		for _, byID := range s.messages {
			consider(byID)
		}
	}
	sortDesc(hits)

	if f.Offset >= len(hits) {
		return nil
	}
	hits = hits[f.Offset:]
	if f.Limit > 0 && len(hits) > f.Limit {
		hits = hits[:f.Limit]
	}
	out := make([]*StoredMessage, len(hits))
	for i, m := range hits {
		out[i] = s.decorateLocked(m)
	}
	return out
}

// findLocked locates a message by ID, optionally within one chat.
func (s *MemoryStore) findLocked(id, chatJID string) *StoredMessage {
	if chatJID != "" {
		return s.messages[chatJID][id]
	}
	for _, byID := range s.messages {
		if m := byID[id]; m != nil {
			return m
		}
	}
	return nil
}

// MessageContext returns a message with the messages around it in its chat.
func (s *MemoryStore) MessageContext(id, chatJID string, before, after int) (*StoredMessage, []*StoredMessage, []*StoredMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	target := s.findLocked(id, chatJID)
	if target == nil {
		return nil, nil, nil, false
	}
	all := make([]*StoredMessage, 0, len(s.messages[target.ChatJID]))
	for _, m := range s.messages[target.ChatJID] {
		all = append(all, m)
	}
	sortAsc(all)
	idx := -1
	for i, m := range all {
		if m.ID == target.ID {
			idx = i
			break
		}
	}
	decorate := func(ms []*StoredMessage) []*StoredMessage {
		out := make([]*StoredMessage, len(ms))
		for i, m := range ms {
			out[i] = s.decorateLocked(m)
		}
		return out
	}
	start := idx - before
	if start < 0 {
		start = 0
	}
	end := idx + 1 + after
	if end > len(all) {
		end = len(all)
	}
	return s.decorateLocked(target), decorate(all[start:idx]), decorate(all[idx+1 : end]), true
}

func (s *MemoryStore) chatViewLocked(c *Chat, includeLast bool) ChatView {
	v := ChatView{JID: c.JID, Name: c.Name, IsGroup: strings.HasSuffix(c.JID, "@g.us")}
	if !c.LastMessageTime.IsZero() {
		t := c.LastMessageTime
		v.LastMessageTime = &t
	}
	if includeLast {
		var newest *StoredMessage
		for _, m := range s.messages[c.JID] {
			if newest == nil || m.Timestamp.After(newest.Timestamp) {
				newest = m
			}
		}
		if newest != nil {
			content, sender, fromMe := newest.Content, newest.Sender, newest.IsFromMe
			v.LastMessage, v.LastSender, v.LastIsFromMe = &content, &sender, &fromMe
		}
	}
	return v
}

// ListChats returns chats matching query (name or JID substring).
func (s *MemoryStore) ListChats(query string, limit, offset int, sortByName, includeLast, directOnly bool) []ChatView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(query)
	chats := make([]*Chat, 0, len(s.chats))
	for _, c := range s.chats {
		if directOnly && strings.HasSuffix(c.JID, "@g.us") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Name), q) && !strings.Contains(strings.ToLower(c.JID), q) {
			continue
		}
		chats = append(chats, c)
	}
	if sortByName {
		sort.Slice(chats, func(i, j int) bool {
			if chats[i].Name == chats[j].Name {
				return chats[i].JID < chats[j].JID
			}
			return chats[i].Name < chats[j].Name
		})
	} else {
		sort.Slice(chats, func(i, j int) bool {
			if chats[i].LastMessageTime.Equal(chats[j].LastMessageTime) {
				return chats[i].JID < chats[j].JID
			}
			return chats[i].LastMessageTime.After(chats[j].LastMessageTime)
		})
	}
	if offset >= len(chats) {
		return nil
	}
	chats = chats[offset:]
	if limit > 0 && len(chats) > limit {
		chats = chats[:limit]
	}
	out := make([]ChatView, len(chats))
	for i, c := range chats {
		out[i] = s.chatViewLocked(c, includeLast)
	}
	return out
}

// GetChat returns one chat view.
func (s *MemoryStore) GetChat(jid string, includeLast bool) (ChatView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.chats[jid]
	if c == nil {
		return ChatView{}, false
	}
	return s.chatViewLocked(c, includeLast), true
}

// ChatsInvolving returns chats where jid is the chat itself or has sent a message.
func (s *MemoryStore) ChatsInvolving(jid string, limit, offset int) []ChatView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var chats []*Chat
	for _, c := range s.chats {
		if c.JID == jid {
			chats = append(chats, c)
			continue
		}
		for _, m := range s.messages[c.JID] {
			if !m.IsFromMe && senderMatches(m.Sender, jid) {
				chats = append(chats, c)
				break
			}
		}
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i].LastMessageTime.After(chats[j].LastMessageTime) })
	if offset >= len(chats) {
		return nil
	}
	chats = chats[offset:]
	if limit > 0 && len(chats) > limit {
		chats = chats[:limit]
	}
	out := make([]ChatView, len(chats))
	for i, c := range chats {
		out[i] = s.chatViewLocked(c, true)
	}
	return out
}

// LastInteraction returns the newest message in the chat with jid or sent by jid.
func (s *MemoryStore) LastInteraction(jid string) (*StoredMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var newest *StoredMessage
	for chatJID, byID := range s.messages {
		for _, m := range byID {
			if chatJID != jid && !(!m.IsFromMe && senderMatches(m.Sender, jid)) {
				continue
			}
			if newest == nil || m.Timestamp.After(newest.Timestamp) {
				newest = m
			}
		}
	}
	if newest == nil {
		return nil, false
	}
	return s.decorateLocked(newest), true
}

// Contact is the API shape of a direct-chat contact.
type Contact struct {
	JID         string `json:"jid"`
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name"`
}

// SearchContacts returns non-group chats whose name or JID contains query.
func (s *MemoryStore) SearchContacts(query string, limit int) []Contact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q := strings.ToLower(query)
	var out []Contact
	for _, c := range s.chats {
		if strings.HasSuffix(c.JID, "@g.us") {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Name), q) && !strings.Contains(strings.ToLower(c.JID), q) {
			continue
		}
		out = append(out, Contact{JID: c.JID, PhoneNumber: strings.SplitN(c.JID, "@", 2)[0], Name: c.Name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].JID < out[j].JID
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// MediaMessage returns the raw stored message (with media info) for download.
func (s *MemoryStore) MediaMessage(id, chatJID string) (*StoredMessage, *mediaInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.findLocked(id, chatJID)
	if m == nil {
		return nil, nil, false
	}
	return m, m.media, true
}

// OldestAnchor returns the anchor for requesting older history in a chat.
func (s *MemoryStore) OldestAnchor(chatJID string) *MessageAnchor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c := s.chats[chatJID]; c != nil && c.Oldest != nil {
		a := *c.Oldest
		return &a
	}
	return nil
}

// ---------------------------------------------------------------------------
// Chat index persistence (metadata only)
// ---------------------------------------------------------------------------

type chatIndexFile struct {
	Version int     `json:"version"`
	Saved   string  `json:"saved"`
	Chats   []*Chat `json:"chats"`
}

// SaveIndex writes chat metadata (no message content) if persistence is on
// and something changed.
func (s *MemoryStore) SaveIndex() error {
	if s.indexPath == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	idx := chatIndexFile{Version: 1, Saved: time.Now().UTC().Format(time.RFC3339)}
	for _, c := range s.chats {
		cc := *c
		idx.Chats = append(idx.Chats, &cc)
	}
	s.dirty = false
	s.mu.Unlock()

	sort.Slice(idx.Chats, func(i, j int) bool { return idx.Chats[i].JID < idx.Chats[j].JID })
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	tmp := s.indexPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath)
}

// LoadIndex restores chat metadata. Missing file is not an error.
func (s *MemoryStore) LoadIndex() (int, error) {
	if s.indexPath == "" {
		return 0, nil
	}
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var idx chatIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		return 0, fmt.Errorf("corrupt chat index %s: %w", filepath.Base(s.indexPath), err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range idx.Chats {
		if c == nil || c.JID == "" {
			continue
		}
		cc := *c
		s.chats[cc.JID] = &cc
		n++
	}
	return n, nil
}

// StartIndexSaver periodically persists the index until stop is closed.
func (s *MemoryStore) StartIndexSaver(interval time.Duration, stop <-chan struct{}, onErr func(error)) {
	if s.indexPath == "" {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.SaveIndex(); err != nil && onErr != nil {
					onErr(err)
				}
			case <-stop:
				return
			}
		}
	}()
}
