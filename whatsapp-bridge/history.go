package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// historyRequester asks the user's phone for older messages in a chat. The
// phone answers asynchronously with an events.HistorySync of type ON_DEMAND;
// the event handler calls notify() and the waiting API request completes.
type historyRequester struct {
	mu      sync.Mutex
	waiters map[string][]chan int // chat JID -> waiters
	timeout time.Duration
}

func newHistoryRequester(timeout time.Duration) *historyRequester {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &historyRequester{waiters: map[string][]chan int{}, timeout: timeout}
}

func (h *historyRequester) register(chatJID string) chan int {
	ch := make(chan int, 1)
	h.mu.Lock()
	h.waiters[chatJID] = append(h.waiters[chatJID], ch)
	h.mu.Unlock()
	return ch
}

func (h *historyRequester) unregister(chatJID string, ch chan int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ws := h.waiters[chatJID]
	for i, w := range ws {
		if w == ch {
			h.waiters[chatJID] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(h.waiters[chatJID]) == 0 {
		delete(h.waiters, chatJID)
	}
}

// notify wakes every request waiting on chatJID with the number of messages
// the phone just delivered for it.
func (h *historyRequester) notify(chatJID string, added int) {
	h.mu.Lock()
	ws := h.waiters[chatJID]
	delete(h.waiters, chatJID)
	h.mu.Unlock()
	for _, w := range ws {
		select {
		case w <- added:
		default:
		}
	}
}

// Waiting reports whether any request is pending (used by the event handler
// to decide whether an ON_DEMAND sync needs to notify anyone).
func (h *historyRequester) Waiting() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waiters) > 0
}

// FetchOlder requests up to count messages older than the oldest cached
// message in chatJID and waits for the phone to deliver them.
func (h *historyRequester) FetchOlder(ctx context.Context, client *whatsmeow.Client, store *MemoryStore, chatJID string, count int) (int, error) {
	if client == nil || !client.IsConnected() || client.Store == nil || client.Store.ID == nil {
		return 0, errors.New("not connected to WhatsApp")
	}
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return 0, fmt.Errorf("invalid chat JID: %v", err)
	}
	anchor := store.OldestAnchor(chatJID)
	if anchor == nil {
		return 0, errors.New("no cached messages for this chat to anchor the request; history can be fetched once at least one message in the chat has been seen")
	}
	if count <= 0 {
		count = 50
	}

	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, IsFromMe: anchor.IsFromMe},
		ID:            anchor.ID,
		Timestamp:     anchor.Timestamp,
	}
	req := client.BuildHistorySyncRequest(info, count)

	ch := h.register(chatJID)
	if _, err := client.SendPeerMessage(ctx, req); err != nil {
		h.unregister(chatJID, ch)
		return 0, fmt.Errorf("failed to send history request to the phone: %v", err)
	}

	select {
	case n := <-ch:
		return n, nil
	case <-time.After(h.timeout):
		h.unregister(chatJID, ch)
		return 0, errors.New("timed out waiting for the phone to respond; make sure it is online and WhatsApp is not force-stopped")
	case <-ctx.Done():
		h.unregister(chatJID, ch)
		return 0, ctx.Err()
	}
}
