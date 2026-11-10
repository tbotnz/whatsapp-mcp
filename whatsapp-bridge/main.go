package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// extractTextContent returns the text of a message, or "".
func extractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if text := msg.GetConversation(); text != "" {
		return text
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

// extractMediaInfo returns the media type, a filename and download info.
func extractMediaInfo(msg *waE2E.Message) (mediaType, filename string, media *mediaInfo) {
	if msg == nil {
		return "", "", nil
	}
	stamp := time.Now().Format("20060102_150405")
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + stamp + ".jpg", &mediaInfo{img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()}
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + stamp + ".mp4", &mediaInfo{vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()}
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + stamp + ".ogg", &mediaInfo{aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		// The filename is chosen by the remote sender: sanitize before storing.
		return "document", sanitizeFilename(doc.GetFileName(), "document_"+stamp), &mediaInfo{doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()}
	}
	return "", "", nil
}

// chatName determines a display name for a chat, preferring what is already
// cached, then the history-sync conversation, then the phone's contacts.
func chatName(ctx context.Context, client *whatsmeow.Client, store *MemoryStore, jid types.JID, conv *waHistorySync.Conversation, pushName string) string {
	if existing := store.ChatName(jid.String()); existing != "" {
		return existing
	}
	if conv != nil {
		if n := conv.GetDisplayName(); n != "" {
			return n
		}
		if n := conv.GetName(); n != "" {
			return n
		}
	}
	if jid.Server == types.GroupServer {
		if client != nil {
			if info, err := client.GetGroupInfo(ctx, jid); err == nil && info.Name != "" {
				return info.Name
			}
		}
		return "Group " + jid.User
	}
	if client != nil && client.Store != nil && client.Store.Contacts != nil {
		if contact, err := client.Store.Contacts.GetContact(ctx, jid); err == nil {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	if pushName != "" {
		return pushName
	}
	return jid.User
}

// handleMessage caches a live incoming or outgoing message.
func handleMessage(client *whatsmeow.Client, store *MemoryStore, msg *events.Message, logger waLog.Logger) {
	content := extractTextContent(msg.Message)
	mediaType, filename, media := extractMediaInfo(msg.Message)
	if content == "" && mediaType == "" {
		return
	}

	chatJID := msg.Info.Chat.String()
	name := chatName(context.Background(), client, store, msg.Info.Chat, nil, msg.Info.PushName)
	store.UpsertChat(chatJID, name, msg.Info.Timestamp)

	store.AddMessage(&StoredMessage{
		ID:        msg.Info.ID,
		ChatJID:   chatJID,
		Sender:    msg.Info.Sender.User,
		Content:   content,
		Timestamp: msg.Info.Timestamp,
		IsFromMe:  msg.Info.IsFromMe,
		MediaType: mediaType,
		Filename:  filename,
		media:     media,
	})

	// Metadata only; message bodies never go to the log.
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

// handleHistorySync caches messages delivered by the phone, both the initial
// sync after pairing and ON_DEMAND responses to fetch_older_messages.
func handleHistorySync(client *whatsmeow.Client, store *MemoryStore, hist *historyRequester, evt *events.HistorySync, logger waLog.Logger) {
	data := evt.Data
	if data == nil {
		return
	}
	syncType := data.GetSyncType()
	logger.Infof("History sync (%s) with %d conversation(s)", syncType, len(data.GetConversations()))

	ownUser := ""
	if client != nil && client.Store != nil && client.Store.ID != nil {
		ownUser = client.Store.ID.User
	}

	total := 0
	for _, conv := range data.GetConversations() {
		chatJIDStr := conv.GetID()
		jid, err := types.ParseJID(chatJIDStr)
		if err != nil {
			continue
		}
		name := chatName(context.Background(), client, store, jid, conv, "")
		added := 0
		for _, hm := range conv.GetMessages() {
			wm := hm.GetMessage()
			if wm == nil || wm.GetMessage() == nil {
				continue
			}
			content := extractTextContent(wm.GetMessage())
			mediaType, filename, media := extractMediaInfo(wm.GetMessage())
			if content == "" && mediaType == "" {
				continue
			}
			ts := wm.GetMessageTimestamp()
			if ts == 0 {
				continue
			}
			key := wm.GetKey()
			isFromMe := key.GetFromMe()
			sender := jid.User
			if isFromMe {
				sender = ownUser
			} else if p := key.GetParticipant(); p != "" {
				if pj, err := types.ParseJID(p); err == nil {
					sender = pj.User
				}
			}
			timestamp := time.Unix(int64(ts), 0)
			store.UpsertChat(chatJIDStr, name, timestamp)
			if store.AddMessage(&StoredMessage{
				ID:        key.GetID(),
				ChatJID:   chatJIDStr,
				Sender:    sender,
				Content:   content,
				Timestamp: timestamp,
				IsFromMe:  isFromMe,
				MediaType: mediaType,
				Filename:  filename,
				media:     media,
			}) {
				added++
			}
		}
		if name != "" {
			store.UpsertChat(chatJIDStr, name, time.Time{})
		}
		total += added
		if syncType == waHistorySync.HistorySync_ON_DEMAND {
			hist.notify(chatJIDStr, added)
		}
	}
	logger.Infof("History sync stored %d new message(s); cache now holds %d", total, store.Count())
}

func main() {
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp bridge (memory-only message cache)...")
	dbLog := waLog.Stdout("Database", "INFO", true)

	sc, err := loadSecurityConfig(logger)
	if err != nil {
		logger.Errorf("%v", err)
		return
	}

	// The only database: whatsmeow's session store (device identity and
	// Signal keys). It is required to stay paired and to decrypt messages.
	ctx := context.Background()
	sessionDBPath := filepath.Join(storeDirName, "whatsapp.db")
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+sessionDBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to open session database: %v", err)
		return
	}
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

	// In-memory message cache, with an optional metadata-only chat index so
	// older history can be requested from the phone after a restart.
	indexPath := ""
	if envBool("WHATSAPP_PERSIST_CHAT_INDEX", true) {
		indexPath = filepath.Join(sc.StoreDir, "chat_index.json")
	}
	store := NewMemoryStore(envInt("WHATSAPP_MAX_CACHED_MESSAGES", 50000), indexPath)
	if n, err := store.LoadIndex(); err != nil {
		logger.Warnf("Could not load chat index: %v", err)
	} else if n > 0 {
		logger.Infof("Loaded %d chat(s) from the index; message bodies will arrive live or via fetch_older_messages", n)
	}
	if _, err := os.Stat(filepath.Join(storeDirName, "messages.db")); err == nil {
		logger.Warnf("Found legacy store/messages.db from an older version. It is no longer used and still contains plaintext message history; delete it when convenient.")
	}

	hist := newHistoryRequester(time.Duration(envInt("WHATSAPP_HISTORY_TIMEOUT", 45)) * time.Second)

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, store, v, logger)
		case *events.HistorySync:
			handleHistorySync(client, store, hist, v, logger)
		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	connected := make(chan bool, 1)
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
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
		if err := client.Connect(); err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	time.Sleep(2 * time.Second)
	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}
	tightenFilePerms(sessionDBPath)
	fmt.Println("\n✓ Connected to WhatsApp!")

	api := &apiServer{
		client: client,
		store:  store,
		sc:     sc,
		hist:   hist,
		logger: logger,
		contacts: func(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
			return client.Store.Contacts.GetAllContacts(ctx)
		},
	}
	srv := api.start()

	stopSaver := make(chan struct{})
	store.StartIndexSaver(15*time.Second, stopSaver, func(err error) { logger.Warnf("Failed to save chat index: %v", err) })

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)
	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")
	<-exitChan

	fmt.Println("Disconnecting...")
	close(stopSaver)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if err := store.SaveIndex(); err != nil {
		logger.Warnf("Failed to save chat index: %v", err)
	}
	client.Disconnect()
}
