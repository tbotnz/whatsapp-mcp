package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func newTestAPI(t *testing.T) (*apiServer, *httptest.Server) {
	t.Helper()
	sc := newTestConfig(t)
	api := &apiServer{
		store:  populated(),
		sc:     sc,
		hist:   newHistoryRequester(time.Second),
		logger: waLog.Noop,
		contacts: func(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
			return map[types.JID]types.ContactInfo{
				types.NewJID("777", types.DefaultUserServer): {Found: true, FullName: "Alicia Phonebook"},
				types.NewJID("111", types.DefaultUserServer): {Found: true, FullName: "Alice From Phone"},
			}, nil
		},
	}
	srv := httptest.NewServer(api.routes())
	t.Cleanup(srv.Close)
	return api, srv
}

func post(t *testing.T, srv *httptest.Server, token, path, body string) (int, map[string]interface{}) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAPIRequiresAuthOnReads(t *testing.T) {
	_, srv := newTestAPI(t)
	for _, path := range []string{"/api/messages", "/api/chats", "/api/contacts", "/api/history", "/api/send"} {
		if code, _ := post(t, srv, "", path, `{}`); code != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", path, code)
		}
	}
}

func TestAPIMessagesAndContext(t *testing.T) {
	api, srv := newTestAPI(t)
	tok := api.sc.Token

	code, out := post(t, srv, tok, "/api/messages", `{"limit": 2}`)
	msgs := out["messages"].([]interface{})
	if code != 200 || len(msgs) != 2 || msgs[0].(map[string]interface{})["id"] != "m3" {
		t.Fatalf("messages: %d %v", code, out)
	}
	first := msgs[0].(map[string]interface{})
	if first["sender_name"] != "Me" || first["chat_name"] != "Alice" {
		t.Errorf("decoration missing: %v", first)
	}
	if _, leaked := first["media"]; leaked {
		t.Error("media info leaked over API")
	}

	code, out = post(t, srv, tok, "/api/messages", `{"query":"ignore","include_context":true,"context_before":1,"context_after":1}`)
	msgs = out["messages"].([]interface{})
	if code != 200 || len(msgs) != 3 {
		t.Fatalf("context expansion: %d %v", code, out)
	}

	code, out = post(t, srv, tok, "/api/messages", `{"after":"not a date"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad date accepted: %d %v", code, out)
	}
	code, out = post(t, srv, tok, "/api/messages", `{"after":"2026-10-20T10:00:00Z","limit":-5,"page":-1}`)
	if code != 200 {
		t.Errorf("clamped ints rejected: %d %v", code, out)
	}
	code, _ = post(t, srv, tok, "/api/messages", `{"limit": 2, "evil": 1}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field accepted: %d", code)
	}

	code, out = post(t, srv, tok, "/api/message_context", `{"message_id":"m2"}`)
	if code != 200 || out["message"].(map[string]interface{})["id"] != "m2" || len(out["before"].([]interface{})) != 1 {
		t.Errorf("message_context: %d %v", code, out)
	}
	if code, _ := post(t, srv, tok, "/api/message_context", `{"message_id":"nope"}`); code != http.StatusNotFound {
		t.Errorf("missing message: %d", code)
	}
}

func TestAPIChatsContactsAndMisc(t *testing.T) {
	api, srv := newTestAPI(t)
	tok := api.sc.Token

	code, out := post(t, srv, tok, "/api/chats", `{"query":"","limit":10}`)
	if code != 200 || len(out["chats"].([]interface{})) != 2 {
		t.Fatalf("chats: %d %v", code, out)
	}
	code, out = post(t, srv, tok, "/api/chats", `{"query":"111","direct_only":true,"limit":1}`)
	chats := out["chats"].([]interface{})
	if code != 200 || len(chats) != 1 || chats[0].(map[string]interface{})["name"] != "Alice" {
		t.Errorf("direct chat lookup: %v", out)
	}
	code, out = post(t, srv, tok, "/api/chat", `{"chat_jid":"nope"}`)
	if code != 200 || out["chat"] != nil {
		t.Errorf("missing chat should be null: %d %v", code, out)
	}

	code, out = post(t, srv, tok, "/api/contacts", `{"query":"ali"}`)
	cs := out["contacts"].([]interface{})
	if code != 200 || len(cs) != 2 {
		t.Fatalf("contacts should merge store and phone: %v", out)
	}
	names := map[string]bool{}
	for _, c := range cs {
		names[c.(map[string]interface{})["name"].(string)] = true
	}
	if !names["Alice"] || !names["Alicia Phonebook"] || names["Alice From Phone"] {
		t.Errorf("merge precedence wrong: %v", names)
	}
	if code, out := post(t, srv, tok, "/api/contacts", `{"query":""}`); code != 200 || len(out["contacts"].([]interface{})) != 0 {
		t.Errorf("empty query must return nothing: %v", out)
	}

	code, out = post(t, srv, tok, "/api/contact_chats", `{"jid":"444@s.whatsapp.net"}`)
	if code != 200 || len(out["chats"].([]interface{})) != 1 {
		t.Errorf("contact_chats: %v", out)
	}
	code, out = post(t, srv, tok, "/api/last_interaction", `{"jid":"111@s.whatsapp.net"}`)
	if code != 200 || out["message"].(map[string]interface{})["id"] != "m3" {
		t.Errorf("last_interaction: %v", out)
	}

	// History without a connected client must fail cleanly, not panic.
	code, out = post(t, srv, tok, "/api/history", `{"chat_jid":"111@s.whatsapp.net","count":50}`)
	if code != http.StatusBadGateway || out["success"] != false {
		t.Errorf("history without client: %d %v", code, out)
	}
	// Send without a client must also fail cleanly and be audited.
	code, out = post(t, srv, tok, "/api/send", `{"recipient":"64211234567","message":"hi"}`)
	if code != http.StatusInternalServerError || out["success"] != false {
		t.Errorf("send without client: %d %v", code, out)
	}
}

func TestHistoryRequesterNotify(t *testing.T) {
	h := newHistoryRequester(time.Second)
	ch := h.register("c@s.whatsapp.net")
	if !h.Waiting() {
		t.Fatal("should be waiting")
	}
	h.notify("other@s.whatsapp.net", 5)
	select {
	case <-ch:
		t.Fatal("woken by wrong chat")
	default:
	}
	h.notify("c@s.whatsapp.net", 7)
	select {
	case n := <-ch:
		if n != 7 {
			t.Errorf("got %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("not notified")
	}
	if h.Waiting() {
		t.Error("waiter not cleared")
	}
	ch2 := h.register("c@s.whatsapp.net")
	h.unregister("c@s.whatsapp.net", ch2)
	if h.Waiting() {
		t.Error("unregister failed")
	}
}
