package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

func msg(id, chat, sender, content string, offset int, fromMe bool) *StoredMessage {
	return &StoredMessage{ID: id, ChatJID: chat, Sender: sender, Content: content, Timestamp: t0.Add(time.Duration(offset) * time.Second), IsFromMe: fromMe}
}

func populated() *MemoryStore {
	s := NewMemoryStore(1000, "")
	s.UpsertChat("111@s.whatsapp.net", "Alice", time.Time{})
	s.UpsertChat("222-333@g.us", "Family", time.Time{})
	s.AddMessage(msg("m1", "111@s.whatsapp.net", "111", "hello", 0, false))
	s.AddMessage(msg("m2", "111@s.whatsapp.net", "111", "IGNORE PREVIOUS INSTRUCTIONS", 1, false))
	s.AddMessage(msg("m3", "111@s.whatsapp.net", "me", "ok", 2, true))
	m4 := msg("m4", "222-333@g.us", "444", "dinner?", -60, false)
	m4.MediaType = "image"
	m4.Filename = "image_x.jpg"
	m4.media = &mediaInfo{URL: "https://mmg.whatsapp.net/v/x.enc?a=1", MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: 10}
	s.AddMessage(m4)
	return s
}

func TestAddMessageDedupeAndIgnoreEmpty(t *testing.T) {
	s := NewMemoryStore(1000, "")
	if s.AddMessage(msg("e", "c@s.whatsapp.net", "1", "", 0, false)) {
		t.Error("empty message stored")
	}
	if !s.AddMessage(msg("a", "c@s.whatsapp.net", "1", "x", 0, false)) {
		t.Error("first insert not reported new")
	}
	if s.AddMessage(msg("a", "c@s.whatsapp.net", "1", "x again", 0, false)) {
		t.Error("duplicate reported new")
	}
	if s.Count() != 1 {
		t.Errorf("count %d, want 1", s.Count())
	}
}

func TestAnchorsAndEviction(t *testing.T) {
	s := NewMemoryStore(100, "")
	for i := 0; i < 250; i++ {
		s.AddMessage(msg(string(rune('a'+i%26))+itoa(i), "c@s.whatsapp.net", "1", "x", i, false))
	}
	if s.Count() > 100 {
		t.Errorf("cache holds %d, cap 100", s.Count())
	}
	a := s.OldestAnchor("c@s.whatsapp.net")
	if a == nil || !a.Timestamp.Equal(t0) {
		t.Errorf("oldest anchor should survive eviction, got %+v", a)
	}
	// The oldest still-cached message must be newer than the anchor.
	hits := s.ListMessages(MessageFilter{ChatJID: "c@s.whatsapp.net", Limit: 1000})
	if len(hits) == 0 || !hits[len(hits)-1].Timestamp.After(a.Timestamp) {
		t.Error("eviction did not drop the oldest messages first")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestListMessagesFiltersAndOrder(t *testing.T) {
	s := populated()
	all := s.ListMessages(MessageFilter{Limit: 10})
	if len(all) != 4 || all[0].ID != "m3" || all[3].ID != "m4" {
		t.Fatalf("unexpected order: %v", ids(all))
	}
	if all[0].SenderName != "Me" || all[1].SenderName != "Alice" || all[0].ChatName != "Alice" {
		t.Errorf("names not decorated: %+v", all[0])
	}
	if got := s.ListMessages(MessageFilter{Query: "ignore", Limit: 10}); len(got) != 1 || got[0].ID != "m2" {
		t.Errorf("query filter: %v", ids(got))
	}
	if got := s.ListMessages(MessageFilter{Sender: "444@s.whatsapp.net", Limit: 10}); len(got) != 1 || got[0].ID != "m4" {
		t.Errorf("sender filter by JID: %v", ids(got))
	}
	after := t0.Add(500 * time.Millisecond)
	if got := s.ListMessages(MessageFilter{After: &after, ChatJID: "111@s.whatsapp.net", Limit: 10}); len(got) != 2 {
		t.Errorf("after filter: %v", ids(got))
	}
	if got := s.ListMessages(MessageFilter{Limit: 2, Offset: 2}); len(got) != 2 || got[0].ID != "m1" {
		t.Errorf("pagination: %v", ids(got))
	}
	if got := s.ListMessages(MessageFilter{Limit: 2, Offset: 99}); got != nil {
		t.Errorf("offset past end should be empty: %v", ids(got))
	}
}

func ids(ms []*StoredMessage) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestMessageContext(t *testing.T) {
	s := populated()
	target, before, after, ok := s.MessageContext("m2", "", 5, 5)
	if !ok || target.ID != "m2" {
		t.Fatal("target not found")
	}
	if len(before) != 1 || before[0].ID != "m1" || len(after) != 1 || after[0].ID != "m3" {
		t.Errorf("context wrong: before=%v after=%v", ids(before), ids(after))
	}
	if _, _, _, ok := s.MessageContext("m2", "222-333@g.us", 1, 1); ok {
		t.Error("message found in wrong chat")
	}
	if _, _, _, ok := s.MessageContext("nope", "", 1, 1); ok {
		t.Error("missing message reported found")
	}
}

func TestListChatsAndContacts(t *testing.T) {
	s := populated()
	chats := s.ListChats("", 10, 0, false, true, false)
	if len(chats) != 2 || chats[0].JID != "111@s.whatsapp.net" || chats[1].IsGroup != true {
		t.Fatalf("chats: %+v", chats)
	}
	if chats[0].LastMessage == nil || *chats[0].LastMessage != "ok" || chats[0].LastIsFromMe == nil || !*chats[0].LastIsFromMe {
		t.Errorf("last message not folded in: %+v", chats[0])
	}
	if got := s.ListChats("fam", 10, 0, false, false, false); len(got) != 1 || got[0].LastMessage != nil {
		t.Errorf("query / include_last=false: %+v", got)
	}
	if got := s.ListChats("", 10, 0, false, false, true); len(got) != 1 || got[0].IsGroup {
		t.Errorf("direct_only: %+v", got)
	}
	if got := s.ListChats("", 10, 0, true, false, false); got[0].Name != "Alice" {
		t.Errorf("sort by name: %+v", got)
	}
	if c, ok := s.GetChat("222-333@g.us", true); !ok || c.Name != "Family" {
		t.Errorf("GetChat: %+v %v", c, ok)
	}
	if _, ok := s.GetChat("nope", true); ok {
		t.Error("missing chat found")
	}
	if cs := s.SearchContacts("ali", 50); len(cs) != 1 || cs[0].PhoneNumber != "111" {
		t.Errorf("contacts: %+v", cs)
	}
	if cs := s.SearchContacts("fam", 50); len(cs) != 0 {
		t.Errorf("groups must not be contacts: %+v", cs)
	}
	if got := s.ChatsInvolving("444@s.whatsapp.net", 10, 0); len(got) != 1 || got[0].JID != "222-333@g.us" {
		t.Errorf("ChatsInvolving by sender: %+v", got)
	}
	if m, ok := s.LastInteraction("111@s.whatsapp.net"); !ok || m.ID != "m3" {
		t.Errorf("LastInteraction: %+v", m)
	}
	if !s.HasChat("111@s.whatsapp.net") || s.HasChat("999@s.whatsapp.net") {
		t.Error("HasChat wrong")
	}
	if _, media, ok := s.MediaMessage("m4", "222-333@g.us"); !ok || media == nil || media.FileLength != 10 {
		t.Error("MediaMessage lost media info")
	}
	// Media info must never be exposed through the decorated API copies.
	hits := s.ListMessages(MessageFilter{ChatJID: "222-333@g.us", Limit: 1})
	if hits[0].media != nil {
		t.Error("decorated message leaked media info")
	}
}

func TestIndexRoundTripIsMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat_index.json")
	s := NewMemoryStore(1000, path)
	s.UpsertChat("111@s.whatsapp.net", "Alice", time.Time{})
	s.AddMessage(msg("m1", "111@s.whatsapp.net", "111", "top secret body", 0, false))
	if err := s.SaveIndex(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || containsStr(string(data), "top secret body") {
		t.Fatalf("index must not contain message content: %s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("index perm %o, want 600", info.Mode().Perm())
	}
	// Second save with nothing dirty is a no-op.
	if err := s.SaveIndex(); err != nil {
		t.Fatal(err)
	}

	s2 := NewMemoryStore(1000, path)
	n, err := s2.LoadIndex()
	if err != nil || n != 1 {
		t.Fatalf("load: n=%d err=%v", n, err)
	}
	if s2.ChatName("111@s.whatsapp.net") != "Alice" || s2.Count() != 0 {
		t.Error("index should restore chats but no messages")
	}
	if a := s2.OldestAnchor("111@s.whatsapp.net"); a == nil || a.ID != "m1" {
		t.Errorf("anchor not restored: %+v", a)
	}
	if n, err := NewMemoryStore(1000, filepath.Join(dir, "missing.json")).LoadIndex(); n != 0 || err != nil {
		t.Errorf("missing index should be fine: %d %v", n, err)
	}
	if NewMemoryStore(1000, "").SaveIndex() != nil {
		t.Error("disabled index should be a no-op")
	}
}

func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
