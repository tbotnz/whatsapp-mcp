package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// sendWhatsAppMessage sends text or a file to recipient after every policy
// check has passed.
func sendWhatsAppMessage(client *whatsmeow.Client, store *MemoryStore, sc *SecurityConfig, recipient string, message string, mediaPath string) (bool, string) {
	if client == nil || !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	recipientJID, err := parseRecipient(recipient)
	if err != nil {
		return false, err.Error()
	}
	if err := sc.recipientAllowed(recipientJID, store.HasChat); err != nil {
		return false, err.Error()
	}

	msg := &waE2E.Message{}

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
		case "jpg", "jpeg":
			mediaType, mimeType = whatsmeow.MediaImage, "image/jpeg"
		case "png":
			mediaType, mimeType = whatsmeow.MediaImage, "image/png"
		case "gif":
			mediaType, mimeType = whatsmeow.MediaImage, "image/gif"
		case "webp":
			mediaType, mimeType = whatsmeow.MediaImage, "image/webp"
		case "ogg":
			mediaType, mimeType = whatsmeow.MediaAudio, "audio/ogg; codecs=opus"
		case "mp4":
			mediaType, mimeType = whatsmeow.MediaVideo, "video/mp4"
		case "avi":
			mediaType, mimeType = whatsmeow.MediaVideo, "video/avi"
		case "mov":
			mediaType, mimeType = whatsmeow.MediaVideo, "video/quicktime"
		default:
			mediaType, mimeType = whatsmeow.MediaDocument, "application/octet-stream"
		}

		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waE2E.ImageMessage{
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
			seconds, waveform, err := analyzeOggOpus(mediaData)
			if err != nil {
				return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
			}
			msg.AudioMessage = &waE2E.AudioMessage{
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
			msg.VideoMessage = &waE2E.VideoMessage{
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
			msg.DocumentMessage = &waE2E.DocumentMessage{
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

	if _, err := client.SendMessage(context.Background(), recipientJID, msg); err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}
	return true, fmt.Sprintf("Message sent to %s", recipientJID.String())
}

// MediaDownloader implements whatsmeow.DownloadableMessage from cached info.
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

// downloadMedia fetches a cached message's attachment into the store directory.
func downloadMedia(client *whatsmeow.Client, store *MemoryStore, sc *SecurityConfig, messageID, chatJID string) (bool, string, string, string, error) {
	msg, media, ok := store.MediaMessage(messageID, chatJID)
	if !ok {
		return false, "", "", "", fmt.Errorf("message not found in the cache (it may have been evicted or never received)")
	}
	if msg.MediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Both the chat JID and the filename originate from the network; build
	// the local path from sanitized components and confine it to the store.
	chatDir := filepath.Join(sc.StoreDir, safePathComponent(msg.ChatJID))
	fallback := msg.MediaType + "_" + safePathComponent(messageID)
	filename := sanitizeFilename(msg.Filename, fallback)
	localPath := filepath.Join(chatDir, filename)
	if !isWithinDir(localPath, sc.StoreDir) || !isWithinDir(chatDir, sc.StoreDir) {
		return false, "", "", "", fmt.Errorf("refusing to write outside the store directory")
	}
	if err := ensurePrivateDir(chatDir); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	if info, err := os.Lstat(localPath); err == nil {
		if !info.Mode().IsRegular() {
			return false, "", "", "", fmt.Errorf("existing media path is not a regular file")
		}
		return true, msg.MediaType, filename, localPath, nil
	}

	if media == nil || media.URL == "" || len(media.MediaKey) == 0 || len(media.FileSHA256) == 0 || len(media.FileEncSHA256) == 0 || media.FileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}
	if media.FileLength > uint64(sc.MaxMediaBytes) {
		return false, "", "", "", fmt.Errorf("media is %d bytes, over the %d byte limit (WHATSAPP_MAX_MEDIA_BYTES)", media.FileLength, sc.MaxMediaBytes)
	}
	if client == nil || !client.IsConnected() {
		return false, "", "", "", fmt.Errorf("not connected to WhatsApp")
	}

	var waMediaType whatsmeow.MediaType
	switch msg.MediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", msg.MediaType)
	}

	downloader := &MediaDownloader{
		URL:           media.URL,
		DirectPath:    extractDirectPathFromURL(media.URL),
		MediaKey:      media.MediaKey,
		FileLength:    media.FileLength,
		FileSHA256:    media.FileSHA256,
		FileEncSHA256: media.FileEncSHA256,
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
	return true, msg.MediaType, filename, localPath, nil
}

// extractDirectPathFromURL extracts the direct path from a WhatsApp media URL.
func extractDirectPathFromURL(url string) string {
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url
	}
	pathPart := strings.SplitN(parts[1], "?", 2)[0]
	return "/" + pathPart
}

// analyzeOggOpus extracts the duration from an Ogg Opus file and generates a
// placeholder waveform. It never indexes past the buffer.
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var sampleRate uint32 = 48000
	var preSkip uint16
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
			break
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 {
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
		duration = uint32(math.Ceil(float64(samples) / float64(sampleRate)))
	} else {
		duration = uint32(float64(len(data)) / 2000.0)
	}
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}
	return duration, placeholderWaveform(duration), nil
}

func minInt(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic 64-byte waveform for voice notes.
func placeholderWaveform(duration uint32) []byte {
	const waveformLength = 64
	waveform := make([]byte, waveformLength)
	rng := mathrand.New(mathrand.NewSource(int64(duration))) // cosmetic only
	baseAmplitude := 35.0
	frequencyFactor := float64(minInt(int(duration), 120)) / 30.0
	for i := range waveform {
		pos := float64(i) / float64(waveformLength)
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)
		val += (rng.Float64() - 0.5) * 15
		val = val*(0.7+0.3*math.Sin(pos*math.Pi)) + 50
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}
		waveform[i] = byte(val)
	}
	return waveform
}
