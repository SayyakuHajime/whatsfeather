// Package whatsapp adapts the pinned unofficial protocol library. Creating an
// Adapter is inert: only explicit auth.connect opens a WhatsApp connection.
package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Adapter struct {
	mu     sync.Mutex
	app    *app.App
	dir    string
	scope  *LiveTestScope
	client *whatsmeow.Client
	store  *sqlstore.Container
	cancel context.CancelFunc
}

func New(a *app.App, dir string) *Adapter { return &Adapter{app: a, dir: dir} }
func NewWithLiveTestScope(a *app.App, dir string, scope *LiveTestScope) *Adapter {
	return &Adapter{app: a, dir: dir, scope: scope}
}

func (a *Adapter) ScopedLiveTest() bool { return a.scope != nil }

func (a *Adapter) scopeSelf(c *whatsmeow.Client) string {
	if c == nil || c.Store == nil {
		return ""
	}
	if c.Store.ID != nil {
		if jid := canonicalSelfJID(*c.Store.ID); jid != "" {
			return jid
		}
	}
	return canonicalSelfJID(c.Store.LID)
}

func canonicalSelfJID(jid types.JID) string {
	jid = jid.ToNonAD()
	if jid.User == "" || jid.Device != 0 || jid.RawAgent != 0 || jid.Integrator != 0 || !isSupportedUserJID(jid) {
		return ""
	}
	return jid.String()
}

func (a *Adapter) scopeSelfIdentities(c *whatsmeow.Client) []string {
	if c == nil || c.Store == nil {
		return nil
	}
	var identities []string
	if c.Store.ID != nil {
		if jid := canonicalSelfJID(*c.Store.ID); jid != "" {
			identities = append(identities, jid)
		}
	}
	if jid := canonicalSelfJID(c.Store.LID); jid != "" && (len(identities) == 0 || identities[0] != jid) {
		identities = append(identities, jid)
	}
	return identities
}

func (a *Adapter) scopeAllowsChat(chat string, c *whatsmeow.Client) bool {
	if a.scope == nil {
		return true
	}
	jid, err := types.ParseJID(chat)
	if err != nil || jid.String() != chat {
		return false
	}
	return a.scope.allowsChat(chat, a.scopeSelfIdentities(c)...)
}

func (a *Adapter) scopeAllowsMessage(chat string, c *whatsmeow.Client, timestamp time.Time) bool {
	return a.scopeAllowsChat(chat, c) && a.scope.allowsTimestamp(timestamp, time.Now())
}

func (a *Adapter) current(c *whatsmeow.Client) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client == c
}
func (a *Adapter) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.Disconnect()
	path := filepath.Join(a.dir, "device.db")
	if st, e := os.Lstat(path); e == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("unsafe device database")
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	f.Close()
	if e = os.Chmod(path, 0600); e != nil {
		return e
	}
	run, cancel := context.WithCancel(ctx)
	store, e := sqlstore.New(run, "sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", nil)
	if e != nil {
		cancel()
		return e
	}
	device, e := store.GetFirstDevice(run)
	if e != nil {
		cancel()
		store.Close()
		return e
	}
	c := whatsmeow.NewClient(device, nil)
	c.EnableAutoReconnect = true
	a.mu.Lock()
	a.client = c
	a.store = store
	a.cancel = cancel
	a.mu.Unlock()
	c.AddEventHandler(func(evt any) {
		if run.Err() == nil && a.current(c) {
			a.event(run, c, evt)
		}
	})
	if c.Store.ID == nil {
		channel, e := c.GetQRChannel(run)
		if e != nil {
			a.Disconnect()
			return e
		}
		go func() {
			for item := range channel {
				if !a.current(c) {
					return
				}
				a.publishQRStatus(run, item.Event, item.Code)
			}
		}()
	}
	if e = c.ConnectContext(run); e != nil {
		a.Disconnect()
		return e
	}
	return nil
}

// publishQRStatus keeps QR rendering testable without opening a QR channel or
// connecting an account. App.SetStatusContext performs the mutation-boundary
// cancellation check after it acquires the app mutex.
func (a *Adapter) publishQRStatus(ctx context.Context, event, code string) bool {
	switch event {
	case "code":
		png, err := qrcode.Encode(code, qrcode.Medium, 256)
		if err != nil {
			return false
		}
		return a.app.SetStatusContext(ctx, "qr", "Scan with a spare account: Settings → Linked devices", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png))
	case "timeout":
		return a.app.SetStatusContext(ctx, "error", "QR expired. Disconnect, then connect again.", "")
	case "success":
		return a.app.SetStatusContext(ctx, "connecting", "Device linked; waiting for connection", "")
	default:
		return false
	}
}

func (a *Adapter) Disconnect() {
	a.mu.Lock()
	c, store, cancel := a.client, a.store, a.cancel
	a.client = nil
	a.store = nil
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if c != nil {
		c.Disconnect()
	}
	if store != nil {
		store.Close()
	}
}
func (a *Adapter) Account(ctx context.Context) (app.Account, error) {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || c.Store == nil || c.Store.ID == nil {
		return app.Account{Available: false, State: "disconnected"}, nil
	}
	jid := c.Store.ID.ToNonAD().String()
	phone := c.Store.ID.User
	name := c.Store.PushName
	if name == "" {
		name = c.Store.BusinessName
	}
	available := c.IsConnected() && c.IsLoggedIn()
	state := "connected"
	if !available {
		state = "disconnected"
	}
	return app.Account{Available: available, DisplayName: name, Phone: phone, JID: jid, Username: "", About: "", AvatarURL: "", State: state}, nil
}

type boundedFile struct {
	*os.File
	limit int64
}

func (f *boundedFile) Write(p []byte) (int, error) {
	position, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if position+int64(len(p)) > f.limit {
		return 0, fmt.Errorf("media exceeds 20 MiB limit")
	}
	return f.File.Write(p)
}
func (f *boundedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.limit {
		return 0, fmt.Errorf("media exceeds 20 MiB limit")
	}
	return f.File.WriteAt(p, off)
}
func (f *boundedFile) Truncate(size int64) error {
	if size > f.limit {
		return fmt.Errorf("media exceeds 20 MiB limit")
	}
	return f.File.Truncate(size)
}

func storedDownloadable(payload []byte, kind string) (whatsmeow.DownloadableMessage, error) {
	var msg waE2E.Message
	if err := proto.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("invalid stored media descriptor")
	}
	if kind == "" {
		switch {
		case msg.ImageMessage != nil:
			return msg.ImageMessage, nil
		case msg.VideoMessage != nil:
			return msg.VideoMessage, nil
		case msg.AudioMessage != nil:
			return msg.AudioMessage, nil
		case msg.DocumentMessage != nil:
			return msg.DocumentMessage, nil
		case msg.StickerMessage != nil:
			return msg.StickerMessage, nil
		}
		return nil, fmt.Errorf("stored message has no downloadable media")
	}
	// New payloads are outer envelopes. The stored kind is also used to make
	// the old typed-payload fallback deterministic rather than trying fields
	// until arbitrary protobuf bytes happen to look useful.
	switch kind {
	case "image":
		if msg.ImageMessage != nil {
			return msg.ImageMessage, nil
		}
	case "sticker":
		if msg.StickerMessage != nil {
			return msg.StickerMessage, nil
		}
	case "video":
		if msg.VideoMessage != nil {
			return msg.VideoMessage, nil
		}
	case "audio":
		if msg.AudioMessage != nil {
			return msg.AudioMessage, nil
		}
	case "document":
		if msg.DocumentMessage != nil {
			return msg.DocumentMessage, nil
		}
	default:
		return nil, fmt.Errorf("stored attachment kind is unsupported")
	}
	// Compatibility for interim profiles that stored the inner message. This
	// branch is reachable only through the trusted persisted kind above.
	var legacy whatsmeow.DownloadableMessage
	switch kind {
	case "image":
		legacy = &waE2E.ImageMessage{}
	case "sticker":
		legacy = &waE2E.StickerMessage{}
	case "video":
		legacy = &waE2E.VideoMessage{}
	case "audio":
		legacy = &waE2E.AudioMessage{}
	case "document":
		legacy = &waE2E.DocumentMessage{}
	}
	if err := proto.Unmarshal(payload, legacy.(proto.Message)); err != nil {
		return nil, fmt.Errorf("invalid stored media descriptor")
	}
	return legacy, nil
}

func (a *Adapter) DownloadMedia(ctx context.Context, payload []byte, destination string) error {
	if a.scope != nil {
		return fmt.Errorf("scoped media requires an allowed profile message lookup")
	}
	return a.downloadMedia(ctx, payload, "", destination)
}

func (a *Adapter) DownloadMediaKind(ctx context.Context, payload []byte, kind, destination string) error {
	if a.scope != nil {
		return fmt.Errorf("scoped media requires an allowed profile message lookup")
	}
	return a.downloadMedia(ctx, payload, kind, destination)
}

func (a *Adapter) AllowManualMedia(chat, id string, timestamp int64) bool {
	if a.scope == nil {
		return true
	}
	if !validScopedMessageID(id) || !a.scope.allowsTimestamp(time.UnixMilli(timestamp), time.Now()) {
		return false
	}
	return a.scopeAllowsChat(chat, a.activeScopeClient())
}

func (a *Adapter) DownloadScopedMedia(ctx context.Context, chat, id string, timestamp int64, payload []byte, kind, destination string) error {
	if a.scope == nil {
		return a.downloadMedia(ctx, payload, kind, destination)
	}
	if !a.AllowManualMedia(chat, id, timestamp) {
		return fmt.Errorf("media is outside the scoped profile lookup")
	}
	return a.downloadMedia(ctx, payload, kind, destination)
}

func (a *Adapter) downloadMedia(ctx context.Context, payload []byte, kind, destination string) error {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return fmt.Errorf("not connected")
	}
	if !a.scope.reserveManualMediaItem() {
		return fmt.Errorf("live-test manual media limit reached")
	}
	downloadable, err := storedDownloadable(payload, kind)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	bounded := &boundedFile{File: file, limit: a.scope.mediaByteLimit()}
	boundedCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.DownloadToFile(boundedCtx, downloadable, bounded); err != nil {
		return err
	}
	// A download may outlive a disconnect/relink. Never let bytes fetched by
	// an obsolete client be reported as a successful current-session result.
	if !a.current(c) || !c.IsConnected() || !c.IsLoggedIn() {
		return fmt.Errorf("WhatsApp client was replaced or disconnected during download")
	}
	return nil
}

// RequestHistory is deliberately explicit: it asks the linked primary device
// for a bounded window immediately before the oldest cached message in one
// conversation. The resulting HistorySync event is the only operation that mutates local rows.
func (a *Adapter) RequestHistory(ctx context.Context, chat string, anchor app.HistoryAnchor, count int) error {
	if a.scope != nil {
		return fmt.Errorf("history requests are disabled for scoped live-test mode")
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if a.scope != nil && (!a.scopeAllowsChat(jid.String(), c) ||
		!a.scope.allowsTimestamp(time.UnixMilli(anchor.Timestamp), time.Now())) {
		return fmt.Errorf("history request is outside the live-test scope")
	}
	if c == nil || !a.current(c) || !c.IsConnected() || !c.IsLoggedIn() {
		return fmt.Errorf("not connected")
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, IsFromMe: anchor.Outgoing},
		ID:            anchor.ID,
		Timestamp:     time.UnixMilli(anchor.Timestamp),
	}
	if count != 50 {
		return fmt.Errorf("history request count is outside the bounded limit")
	}
	request := c.BuildHistorySyncRequest(&info, 50)
	_, err = c.SendPeerMessage(ctx, request)
	if err != nil {
		return err
	}
	if !a.current(c) {
		return fmt.Errorf("WhatsApp client was replaced during history request")
	}
	return nil
}

// mediaSendUnknown marks a SendMessage error as ambiguous. Upload failures
// remain ordinary errors and are safe to report as failed before any message
// was accepted by the server.
type mediaSendUnknown struct{ err error }

func (e mediaSendUnknown) Error() string        { return e.err.Error() }
func (e mediaSendUnknown) Unwrap() error        { return e.err }
func (e mediaSendUnknown) UnknownOutcome() bool { return true }

func validateAudioMetadata(meta app.Attachment, caption string) (int64, error) {
	if caption != "" {
		return 0, fmt.Errorf("audio messages do not support captions")
	}
	mime := strings.ToLower(strings.TrimSpace(meta.MIME))
	if meta.Kind != "audio" || !strings.HasPrefix(mime, "audio/") || meta.Size <= 0 {
		return 0, fmt.Errorf("invalid audio attachment metadata")
	}
	if meta.IsVoiceNote && mime != "audio/ogg; codecs=opus" {
		return 0, fmt.Errorf("voice notes require validated OGG/Opus audio")
	}
	if meta.Duration < 0 || meta.Duration > int64(^uint32(0)) {
		return 0, fmt.Errorf("audio duration is out of range")
	}
	wantLength := meta.PreparedSize
	if wantLength == 0 {
		wantLength = meta.Size
	}
	if wantLength <= 0 {
		return 0, fmt.Errorf("invalid prepared audio byte count")
	}
	return wantLength, nil
}

func buildAudioMessage(upload whatsmeow.UploadResponse, meta app.Attachment, caption string) (*waE2E.Message, error) {
	wantLength, err := validateAudioMetadata(meta, caption)
	if err != nil {
		return nil, err
	}
	if upload.FileLength != uint64(wantLength) {
		return nil, fmt.Errorf("uploaded audio byte count does not match staged file")
	}
	return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		URL: proto.String(upload.URL), DirectPath: proto.String(upload.DirectPath),
		MediaKey: upload.MediaKey, FileEncSHA256: upload.FileEncSHA256,
		FileSHA256: upload.FileSHA256, FileLength: proto.Uint64(upload.FileLength),
		Mimetype: proto.String(meta.MIME), Seconds: proto.Uint32(uint32(meta.Duration)),
		PTT: proto.Bool(meta.IsVoiceNote),
	}}, nil
}

func buildVideoMessage(url, directPath string, mediaKey, fileEncSHA256, fileSHA256 []byte, fileLength uint64, mime, caption string) *waE2E.Message {
	return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
		URL: proto.String(url), DirectPath: proto.String(directPath),
		MediaKey: mediaKey, FileEncSHA256: fileEncSHA256,
		FileSHA256: fileSHA256, FileLength: proto.Uint64(fileLength),
		Mimetype: proto.String(mime), Caption: proto.String(caption),
	}}
}

func validateStickerMetadata(meta app.Attachment, caption string) error {
	if caption != "" {
		return fmt.Errorf("sticker messages do not support captions")
	}
	if meta.Kind != "sticker" || meta.MIME != "image/webp" || meta.Width != 512 || meta.Height != 512 || meta.Size <= 0 || meta.Size > app.StickerMaxBytes {
		return fmt.Errorf("invalid static sticker metadata")
	}
	if meta.PreparedSize != 0 && meta.PreparedSize != meta.Size {
		return fmt.Errorf("sticker byte counts do not match")
	}
	return nil
}

func buildStickerMessage(upload whatsmeow.UploadResponse, meta app.Attachment, caption string) (*waE2E.Message, error) {
	if err := validateStickerMetadata(meta, caption); err != nil {
		return nil, err
	}
	if upload.FileLength != uint64(meta.Size) {
		return nil, fmt.Errorf("uploaded sticker byte count does not match staged file")
	}
	return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
		URL: proto.String(upload.URL), DirectPath: proto.String(upload.DirectPath),
		MediaKey: upload.MediaKey, FileEncSHA256: upload.FileEncSHA256,
		FileSHA256: upload.FileSHA256, FileLength: proto.Uint64(upload.FileLength),
		Mimetype: proto.String("image/webp"), Height: proto.Uint32(uint32(meta.Height)),
		Width: proto.Uint32(uint32(meta.Width)), IsAnimated: proto.Bool(false),
	}}, nil
}

type mediaSendClient interface {
	IsConnected() bool
	IsLoggedIn() bool
	UploadReader(context.Context, io.Reader, io.ReadWriteSeeker, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

const mediaTransferLimit int64 = 20 << 20

type protocolMessageSender interface {
	IsConnected() bool
	IsLoggedIn() bool
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

func scopedOutboundTarget(scope *LiveTestScope, chat, self string, connected, loggedIn bool) (types.JID, error) {
	jid, err := types.ParseJID(chat)
	if err != nil || jid.String() != chat {
		return types.EmptyJID, fmt.Errorf("invalid outbound chat")
	}
	if scope != nil {
		selfJID, selfErr := types.ParseJID(self)
		if !connected || !loggedIn || selfErr != nil || selfJID.String() != self ||
			jid.ToNonAD().String() != selfJID.ToNonAD().String() || !scope.allowsChat(chat, self) {
			return types.EmptyJID, fmt.Errorf("scoped outbound target is not the connected self chat")
		}
	}
	return jid, nil
}

func sendProtocolAtBoundary(ctx context.Context, scope *LiveTestScope, sender protocolMessageSender, self, chat string, message *waE2E.Message, id string, isCurrent func() bool) (int64, error) {
	if sender == nil || !sender.IsConnected() || !sender.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	jid, err := scopedOutboundTarget(scope, chat, self, sender.IsConnected(), sender.IsLoggedIn())
	if err != nil {
		return 0, err
	}
	response, err := sender.SendMessage(ctx, jid, message, whatsmeow.SendRequestExtra{ID: id, Timeout: 20 * time.Second})
	if err != nil {
		return 0, err
	}
	current := isCurrent == nil || isCurrent()
	if !current {
		return 0, fmt.Errorf("WhatsApp client was replaced during send")
	}
	if scope != nil {
		scope.rememberSuccessfulSend(chat, string(response.ID), current)
	}
	return response.Timestamp.UnixMilli(), nil
}

func (a *Adapter) selfForSender(sender protocolMessageSender) string {
	if client, ok := sender.(*whatsmeow.Client); ok {
		return a.scopeSelf(client)
	}
	return ""
}

func (a *Adapter) requireScopedMutationTarget(chat, id string) error {
	if a.scope != nil && !a.scope.allowsMutationTarget(chat, id) {
		return fmt.Errorf("scoped remote mutation target was not sent successfully in this run")
	}
	return nil
}

func validateMediaSnapshot(ctx context.Context, file *os.File, meta app.Attachment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if meta.Size <= 0 || meta.Size > mediaTransferLimit || meta.SHA256 == "" {
		return fmt.Errorf("media metadata is missing a valid size or SHA-256 snapshot")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Size {
		return fmt.Errorf("media file no longer matches its validated size snapshot")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, mediaTransferLimit+1))
	if err != nil || n != meta.Size || n > mediaTransferLimit || hex.EncodeToString(hash.Sum(nil)) != meta.SHA256 {
		return fmt.Errorf("media file no longer matches its validated SHA-256 snapshot")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return ctx.Err()
}

func (a *Adapter) SendMedia(ctx context.Context, chat, path string, meta app.Attachment, caption, id string) (int64, error) {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	return a.sendMediaWithClient(ctx, c, func() bool { return a.current(c) }, chat, path, meta, caption, id)
}

func (a *Adapter) sendMediaWithClient(ctx context.Context, c mediaSendClient, isCurrent func() bool, chat, path string, meta app.Attachment, caption, id string) (int64, error) {
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	self := a.selfForSender(c)
	if _, err := scopedOutboundTarget(a.scope, chat, self, c.IsConnected(), c.IsLoggedIn()); err != nil {
		return 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err = validateMediaSnapshot(sendCtx, file, meta); err != nil {
		return 0, err
	}
	mediaType := whatsmeow.MediaImage
	var message *waE2E.Message
	if meta.Kind == "gif" {
		encoded, transcodeErr := animatedGIFMP4(sendCtx, file, a.dir)
		if transcodeErr != nil {
			return 0, transcodeErr
		}
		file.Close()
		file, err = os.Open(encoded)
		if err != nil {
			return 0, err
		}
		defer func() { file.Close(); _ = os.Remove(encoded) }()
		mediaType = whatsmeow.MediaVideo
		resp, uploadErr := c.UploadReader(sendCtx, file, nil, mediaType)
		if uploadErr != nil {
			return 0, uploadErr
		}
		message = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{URL: proto.String(resp.URL), DirectPath: proto.String(resp.DirectPath), MediaKey: resp.MediaKey, FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: proto.Uint64(resp.FileLength), Mimetype: proto.String("video/mp4"), Caption: proto.String(caption), GifPlayback: proto.Bool(true)}}
	} else {
		switch meta.Kind {
		case "video":
			if meta.MIME != "video/mp4" || meta.VideoCodec != "h264" || (meta.AudioCodec != "" && meta.AudioCodec != "aac") {
				return 0, fmt.Errorf("live video send requires validated MP4 with H.264 and optional AAC-LC")
			}
			mediaType = whatsmeow.MediaVideo
			resp, uploadErr := c.UploadReader(sendCtx, file, nil, mediaType)
			if uploadErr != nil {
				return 0, uploadErr
			}
			message = buildVideoMessage(resp.URL, resp.DirectPath, resp.MediaKey, resp.FileEncSHA256, resp.FileSHA256, resp.FileLength, "video/mp4", caption)
		case "sticker":
			if err := validateStickerMetadata(meta, caption); err != nil {
				return 0, err
			}
			info, statErr := file.Stat()
			if statErr != nil {
				return 0, statErr
			}
			if info.Size() != meta.Size || info.Size() > app.StickerMaxBytes {
				return 0, fmt.Errorf("sticker file size changed before upload")
			}
			mediaType = whatsmeow.MediaImage
			resp, uploadErr := c.UploadReader(sendCtx, file, nil, mediaType)
			if uploadErr != nil {
				return 0, uploadErr
			}
			message, err = buildStickerMessage(resp, meta, caption)
			if err != nil {
				return 0, err
			}
		case "image", "gif":
			mediaType = whatsmeow.MediaImage
			resp, uploadErr := c.UploadReader(sendCtx, file, nil, mediaType)
			if uploadErr != nil {
				return 0, uploadErr
			}
			message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				URL: proto.String(resp.URL), DirectPath: proto.String(resp.DirectPath),
				MediaKey: resp.MediaKey, FileEncSHA256: resp.FileEncSHA256,
				FileSHA256: resp.FileSHA256, FileLength: proto.Uint64(resp.FileLength),
				Mimetype: proto.String(meta.MIME), Caption: proto.String(caption),
			}}
		case "audio":
			wantLength, validateErr := validateAudioMetadata(meta, caption)
			if validateErr != nil {
				return 0, validateErr
			}
			info, statErr := file.Stat()
			if statErr != nil {
				return 0, statErr
			}
			if info.Size() != wantLength {
				return 0, fmt.Errorf("audio file size changed before upload")
			}
			mediaType = whatsmeow.MediaAudio
			resp, uploadErr := c.UploadReader(sendCtx, file, nil, mediaType)
			if uploadErr != nil {
				return 0, uploadErr
			}
			message, err = buildAudioMessage(resp, meta, caption)
			if err != nil {
				return 0, err
			}
		case "document":
			mediaType = whatsmeow.MediaDocument
			resp, uploadErr := c.UploadReader(sendCtx, file, nil, mediaType)
			if uploadErr != nil {
				return 0, uploadErr
			}
			message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				URL: proto.String(resp.URL), DirectPath: proto.String(resp.DirectPath),
				MediaKey: resp.MediaKey, FileEncSHA256: resp.FileEncSHA256,
				FileSHA256: resp.FileSHA256, FileLength: proto.Uint64(resp.FileLength),
				Mimetype: proto.String(meta.MIME), FileName: proto.String(meta.Name),
				Title: proto.String(meta.Name), Caption: proto.String(caption),
			}}
		default:
			return 0, fmt.Errorf("live sending does not support %s attachments", meta.Kind)
		}
	}
	if !isCurrent() || !c.IsConnected() || !c.IsLoggedIn() {
		return 0, mediaSendUnknown{fmt.Errorf("WhatsApp client was replaced or disconnected before media send")}
	}
	stamp, sendErr := sendProtocolAtBoundary(sendCtx, a.scope, c, self, chat, message, id, isCurrent)
	if sendErr != nil {
		return 0, mediaSendUnknown{sendErr}
	}
	return stamp, nil
}

func animatedGIFMP4(ctx context.Context, input *os.File, dir string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("animated GIF sending is unsupported: ffmpeg is unavailable")
	}
	root := filepath.Join(dir, "media")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(root, "gif-*.mp4")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	_ = out.Close()
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(outPath)
		}
	}()
	if _, err = input.Seek(0, 0); err != nil {
		return "", err
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, "ffmpeg", "-hide_banner", "-loglevel", "error", "-threads", "2", "-t", "15", "-i", "/dev/stdin", "-vf", "scale=w=max(2\\,min(640\\,iw)):h=max(2\\,min(640\\,ih)):force_original_aspect_ratio=decrease:force_divisible_by=2", "-an", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-fs", fmt.Sprint(20<<20), "-y", outPath)
	cmd.Stdin = input
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", fmt.Errorf("animated GIF encoding failed: %s", strings.TrimSpace(string(output)))
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 || st.Size() > 20<<20 {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("animated GIF encoding exceeded the media limit")
	}
	complete = true
	return outPath, nil
}

type contactSendClient interface {
	IsConnected() bool
	IsLoggedIn() bool
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

func buildContactProtocolMessage(cards []app.ContactCard) (*waE2E.Message, error) {
	if len(cards) == 0 || len(cards) > app.ContactCardMaxCount {
		return nil, fmt.Errorf("contact count is outside the bounded limit")
	}
	contacts := make([]*waE2E.ContactMessage, 0, len(cards))
	totalBytes := 0
	for _, card := range cards {
		vcard, err := app.CanonicalVCard(card)
		if err != nil {
			return nil, fmt.Errorf("invalid contact projection")
		}
		totalBytes += len(vcard)
		if totalBytes > app.ContactVCardMaxBytes {
			return nil, fmt.Errorf("canonical contact cards exceed the bounded limit")
		}
		parsed, err := app.ParseVCard([]byte(vcard))
		if err != nil || len(parsed) != 1 {
			return nil, fmt.Errorf("invalid canonical contact projection")
		}
		contacts = append(contacts, &waE2E.ContactMessage{DisplayName: proto.String(parsed[0].Name), Vcard: proto.String(vcard)})
	}
	if len(contacts) == 1 {
		return &waE2E.Message{ContactMessage: contacts[0]}, nil
	}
	return &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
		DisplayName: proto.String("Contacts"), Contacts: contacts,
	}}, nil
}

func sendContactsWithClient(ctx context.Context, client contactSendClient, chat string, cards []app.ContactCard, id string) (int64, error) {
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		return 0, err
	}
	message, err := buildContactProtocolMessage(cards)
	if err != nil {
		return 0, err
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := client.SendMessage(bounded, jid, message, whatsmeow.SendRequestExtra{ID: id, Timeout: 20 * time.Second})
	if err != nil {
		return 0, err
	}
	return response.Timestamp.UnixMilli(), nil
}

func (a *Adapter) SendContacts(ctx context.Context, chat string, cards []app.ContactCard, id string) (int64, error) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	message, err := buildContactProtocolMessage(cards)
	if err != nil {
		return 0, err
	}
	return sendProtocolAtBoundary(ctx, a.scope, client, a.scopeSelf(client), chat, message, id, func() bool { return a.current(client) })
}

func buildStaticLocationMessage(location app.LocationProjection) (*waE2E.Message, error) {
	if location.IsLive || !app.LocationProjectionValid(location) {
		return nil, fmt.Errorf("invalid static location projection")
	}
	return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude: proto.Float64(location.Latitude), DegreesLongitude: proto.Float64(location.Longitude),
		IsLive: proto.Bool(false), Comment: optionalString(location.Caption),
		Name: optionalString(location.Name), Address: optionalString(location.Address),
	}}, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return proto.String(value)
}

func sendLocationWithClient(ctx context.Context, client contactSendClient, chat string, location app.LocationProjection, id string) (int64, error) {
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		return 0, err
	}
	message, err := buildStaticLocationMessage(location)
	if err != nil {
		return 0, err
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := client.SendMessage(bounded, jid, message, whatsmeow.SendRequestExtra{ID: id, Timeout: 20 * time.Second})
	if err != nil {
		return 0, err
	}
	return response.Timestamp.UnixMilli(), nil
}

func (a *Adapter) SendLocation(ctx context.Context, chat string, location app.LocationProjection, id string) (int64, error) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	message, err := buildStaticLocationMessage(location)
	if err != nil {
		return 0, err
	}
	return sendProtocolAtBoundary(ctx, a.scope, client, a.scopeSelf(client), chat, message, id, func() bool { return a.current(client) })
}

func (a *Adapter) Send(ctx context.Context, chat, text, id string) (int64, error) {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return sendProtocolAtBoundary(bounded, a.scope, c, a.scopeSelf(c), chat, &waE2E.Message{Conversation: proto.String(text)}, id, func() bool { return a.current(c) })
}

func messageKey(chat, targetID string, fromMe bool, participant string) (*waCommon.MessageKey, error) {
	jid, err := types.ParseJID(chat)
	if err != nil || targetID == "" {
		return nil, fmt.Errorf("invalid message key")
	}
	key := &waCommon.MessageKey{RemoteJID: proto.String(jid.ToNonAD().String()), ID: proto.String(targetID), FromMe: proto.Bool(fromMe)}
	if jid.Server == types.GroupServer {
		if participant == "" {
			return nil, fmt.Errorf("group message key requires the original sender")
		}
		participantJID, parseErr := types.ParseJID(participant)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid message participant")
		}
		key.Participant = proto.String(participantJID.ToNonAD().String())
	}
	return key, nil
}

func buildReplyMessage(chat, targetID string, fromMe bool, participant, text, quotedBody string) (*waE2E.Message, error) {
	key, err := messageKey(chat, targetID, fromMe, participant)
	if err != nil {
		return nil, err
	}
	info := &waE2E.ContextInfo{StanzaID: proto.String(key.GetID()), QuotedMessage: &waE2E.Message{Conversation: proto.String(quotedBody)}}
	if key.GetParticipant() != "" {
		info.Participant = proto.String(key.GetParticipant())
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: info}}, nil
}

func buildReactionMessage(chat, targetID string, fromMe bool, participant, emoji string) (*waE2E.Message, error) {
	key, err := messageKey(chat, targetID, fromMe, participant)
	if err != nil {
		return nil, err
	}
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: key, Text: proto.String(emoji), SenderTimestampMS: proto.Int64(time.Now().UnixMilli())}}, nil
}

func buildEditMessage(chat, targetID, text string) (*waE2E.Message, error) {
	key, err := messageKey(chat, targetID, true, "")
	if err != nil {
		return nil, err
	}
	typ := waE2E.ProtocolMessage_MESSAGE_EDIT
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: key, Type: &typ, TimestampMS: proto.Int64(time.Now().UnixMilli()), EditedMessage: &waE2E.Message{Conversation: proto.String(text)}}}, nil
}

func buildRevokeMessage(chat, targetID string) (*waE2E.Message, error) {
	key, err := messageKey(chat, targetID, true, "")
	if err != nil {
		return nil, err
	}
	typ := waE2E.ProtocolMessage_REVOKE
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: key, Type: &typ}}, nil
}

func buildForwardMessage(chat, text string) (*waE2E.Message, error) {
	if _, err := types.ParseJID(chat); err != nil || text == "" {
		return nil, fmt.Errorf("invalid forwarded text")
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)}}}, nil
}

func (a *Adapter) sendAction(ctx context.Context, chat, targetID, id string, message *waE2E.Message) (int64, error) {
	if targetID != "" {
		if err := a.requireScopedMutationTarget(chat, targetID); err != nil {
			return 0, err
		}
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return 0, fmt.Errorf("not connected")
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return sendProtocolAtBoundary(bounded, a.scope, c, a.scopeSelf(c), chat, message, id, func() bool { return a.current(c) })
}

func (a *Adapter) SendReply(ctx context.Context, chat, targetID string, fromMe bool, participant, quotedBody, text, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(chat, targetID); err != nil {
		return 0, err
	}
	message, err := buildReplyMessage(chat, targetID, fromMe, participant, text, quotedBody)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, chat, targetID, id, message)
}
func (a *Adapter) SendListReply(ctx context.Context, target app.Message, rowID, title, description, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(target.ChatID, target.ID); err != nil {
		return 0, err
	}
	message, err := buildListResponseMessage(target, rowID, title, description)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, target.ChatID, target.ID, id, message)
}
func (a *Adapter) SendButtonReply(ctx context.Context, target app.Message, buttonID, displayText, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(target.ChatID, target.ID); err != nil {
		return 0, err
	}
	message, err := buildButtonsResponseMessage(target, buttonID, displayText)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, target.ChatID, target.ID, id, message)
}
func (a *Adapter) SendReaction(ctx context.Context, chat, targetID string, fromMe bool, participant, emoji, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(chat, targetID); err != nil {
		return 0, err
	}
	message, err := buildReactionMessage(chat, targetID, fromMe, participant, emoji)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, chat, targetID, id, message)
}
func (a *Adapter) EditMessage(ctx context.Context, chat, targetID, text, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(chat, targetID); err != nil {
		return 0, err
	}
	message, err := buildEditMessage(chat, targetID, text)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, chat, targetID, id, message)
}
func (a *Adapter) RevokeMessage(ctx context.Context, chat, targetID, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(chat, targetID); err != nil {
		return 0, err
	}
	message, err := buildRevokeMessage(chat, targetID)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, chat, targetID, id, message)
}
func (a *Adapter) ForwardText(ctx context.Context, chat, text, id string) (int64, error) {
	if a.scope != nil {
		return 0, fmt.Errorf("historical forwarding is disabled for scoped live-test mode")
	}
	message, err := buildForwardMessage(chat, text)
	if err != nil {
		return 0, err
	}
	return a.sendAction(ctx, chat, "", id, message)
}

func numericContactUser(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSupportedUserJID(jid types.JID) bool {
	switch jid.Server {
	case types.DefaultUserServer, types.LegacyUserServer, types.HiddenUserServer, types.HostedServer, types.HostedLIDServer:
		return jid.User != ""
	}
	return false
}

func (a *Adapter) ResolveContact(ctx context.Context, phone string) (app.Contact, bool, error) {
	if a.scope != nil {
		return app.Contact{}, false, fmt.Errorf("contact resolution is disabled for scoped live-test mode")
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return app.Contact{}, false, fmt.Errorf("not connected")
	}
	result, err := c.IsOnWhatsApp(ctx, []string{phone})
	if err != nil {
		return app.Contact{}, false, err
	}
	if len(result) == 0 || !result[0].IsIn {
		return app.Contact{}, false, nil
	}
	jid := result[0].JID.ToNonAD()
	if !isSupportedUserJID(jid) {
		return app.Contact{}, false, fmt.Errorf("phone lookup returned a non-user JID")
	}
	name := ""
	if result[0].VerifiedName != nil && result[0].VerifiedName.Details != nil {
		name = result[0].VerifiedName.Details.GetVerifiedName()
	}
	if c.Store != nil && c.Store.Contacts != nil {
		if contact, lookupErr := c.Store.Contacts.GetContact(ctx, jid); lookupErr == nil && contact.Found {
			name = firstText(contact.FullName, contact.FirstName, contact.BusinessName, contact.PushName, name)
		}
	}
	return app.Contact{JID: jid.String(), Name: name, Phone: phone, Verified: true}, true, nil
}

func (a *Adapter) ListContacts(ctx context.Context) ([]app.Contact, error) {
	if a.scope != nil {
		return nil, fmt.Errorf("contact roster is disabled for scoped live-test mode")
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() || c.Store == nil || c.Store.Contacts == nil {
		return nil, fmt.Errorf("cached contact roster unavailable")
	}
	contacts, err := c.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]app.Contact, 0, len(contacts))
	for jid, info := range contacts {
		jid = jid.ToNonAD()
		if !isSupportedUserJID(jid) || !info.Found {
			continue
		}
		phone := ""
		if jid.Server == types.DefaultUserServer || jid.Server == types.LegacyUserServer {
			if numericContactUser(jid.User) {
				phone = jid.User
			}
		}
		result = append(result, app.Contact{JID: jid.String(), Name: firstText(info.FullName, info.FirstName, info.BusinessName, info.PushName), Phone: phone, Verified: false})
	}
	return result, nil
}

// communityGroupsFromSDK keeps only the fields needed for a folder projection.
// The announcement bit is considered known only for an SDK-identified default
// subgroup, and true only when that subgroup is also in announce mode.
func communityGroupsFromSDK(groups []*types.GroupInfo) []app.CommunityGroupInfo {
	result := make([]app.CommunityGroupInfo, 0, len(groups))
	for _, group := range groups {
		if group == nil {
			continue
		}
		result = append(result, app.CommunityGroupInfo{
			ID:                  group.JID.String(),
			Title:               group.Name,
			IsParent:            group.GroupParent.IsParent,
			LinkedParentID:      group.GroupLinkedParent.LinkedParentJID.String(),
			IsAnnouncement:      group.GroupIsDefaultSub.IsDefaultSubGroup && group.GroupAnnounce.IsAnnounce,
			IsAnnouncementKnown: group.GroupIsDefaultSub.IsDefaultSubGroup,
		})
	}
	return result
}

// GetJoinedGroups performs no work until the explicit community.refresh route
// calls it. Only group IDs, names, and SDK community-link flags are projected.
func (a *Adapter) GetJoinedGroups(ctx context.Context) ([]app.CommunityGroupInfo, error) {
	if a.scope != nil {
		return nil, fmt.Errorf("community refresh is disabled for scoped live-test mode")
	}
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("community refresh canceled")
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return nil, fmt.Errorf("not connected")
	}
	groups, err := c.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	if !a.current(c) || !c.IsConnected() || !c.IsLoggedIn() {
		return nil, fmt.Errorf("WhatsApp client was replaced or disconnected during community refresh")
	}
	return communityGroupsFromSDK(groups), nil
}

func messageText(evt *events.Message) string {
	content := messageContent(evt)
	return content.body
}

type extractedContent struct {
	body, kind               string
	attachment               *app.Attachment
	payload                  []byte
	contacts                 []app.ContactCard
	location                 *app.LocationProjection
	poll                     *app.PollProjection
	pollResults              *app.PollResultsProjection
	list                     *app.ListProjection
	listActionRows           []app.ListActionRow
	buttons                  *app.ButtonsProjection
	buttonActionRows         []app.ButtonActionRow
	interactive              *app.TextCardProjection
	template                 *app.TextCardProjection
	linkPreview              *app.LinkPreviewProjection
	quotedBody, quotedSender string
}

func mediaAttachment(kind, name, mime string, size uint64, hash []byte, duration uint32, width, height uint32, msg proto.Message) (*app.Attachment, []byte) {
	if mime == "" {
		mime = "application/octet-stream"
	}
	attachment := &app.Attachment{Name: name, Size: int64(size), MIME: mime, Kind: kind, State: "remote"}
	if len(hash) > 0 {
		attachment.SHA256 = hex.EncodeToString(hash)
	}
	if duration > 0 {
		attachment.Duration = int64(duration)
	}
	if width > 0 {
		attachment.Width = int(width)
	}
	if height > 0 {
		attachment.Height = int(height)
	}
	// The downloader consumes a waE2E.Message, not the media submessage.
	// Keep the envelope construction explicit by the trusted extracted kind;
	// this also prevents ambiguous legacy trial-deserialization.
	var envelope *waE2E.Message
	switch kind {
	case "image":
		if typed, ok := msg.(*waE2E.ImageMessage); ok {
			envelope = &waE2E.Message{ImageMessage: typed}
		}
	case "sticker":
		if typed, ok := msg.(*waE2E.StickerMessage); ok {
			envelope = &waE2E.Message{StickerMessage: typed}
		}
	case "video":
		if typed, ok := msg.(*waE2E.VideoMessage); ok {
			envelope = &waE2E.Message{VideoMessage: typed}
		}
	case "audio":
		if typed, ok := msg.(*waE2E.AudioMessage); ok {
			envelope = &waE2E.Message{AudioMessage: typed}
		}
	case "document":
		if typed, ok := msg.(*waE2E.DocumentMessage); ok {
			envelope = &waE2E.Message{DocumentMessage: typed}
		}
	}
	if envelope == nil {
		return attachment, nil
	}
	payload, _ := proto.Marshal(envelope)
	return attachment, payload
}

func unsupportedContent() extractedContent {
	return extractedContent{body: "[Unsupported content — open the official app]", kind: "unsupported"}
}

func unsupportedLocationContent() extractedContent {
	return unsupportedContent()
}

func projectedLocationContent(location app.LocationProjection) extractedContent {
	if !app.LocationProjectionValid(location) {
		return unsupportedLocationContent()
	}
	return extractedContent{body: "Location", kind: "location", location: &location}
}

func projectedPollContent(protocolPoll *waE2E.PollCreationMessage) extractedContent {
	if protocolPoll == nil || len(protocolPoll.GetOptions()) == 0 || len(protocolPoll.GetOptions()) > app.PollOptionMaxCount {
		return unsupportedContent()
	}
	projection := app.PollProjection{
		Question: protocolPoll.GetName(),
		Options:  make([]string, 0, len(protocolPoll.GetOptions())),
	}
	if protocolPoll.SelectableOptionsCount != nil {
		count := protocolPoll.GetSelectableOptionsCount()
		projection.SelectableOptionsCount = &count
	}
	for _, option := range protocolPoll.GetOptions() {
		if option == nil {
			return unsupportedContent()
		}
		projection.Options = append(projection.Options, option.GetOptionName())
	}
	if !app.PollProjectionValid(projection) {
		return unsupportedContent()
	}
	return extractedContent{body: projection.Question, kind: "poll", poll: &projection}
}

func projectedPollResultsContent(snapshot *waE2E.PollResultSnapshotMessage) extractedContent {
	if snapshot == nil || snapshot.Name == nil || len(snapshot.GetPollVotes()) == 0 || len(snapshot.GetPollVotes()) > app.PollOptionMaxCount {
		return unsupportedContent()
	}
	projection := app.PollResultsProjection{
		Question: snapshot.GetName(),
		Options:  make([]app.PollResultOption, 0, len(snapshot.GetPollVotes())),
	}
	for _, vote := range snapshot.GetPollVotes() {
		if vote == nil || vote.OptionName == nil || vote.OptionVoteCount == nil {
			return unsupportedContent()
		}
		projection.Options = append(projection.Options, app.PollResultOption{Name: vote.GetOptionName(), VoteCount: vote.GetOptionVoteCount()})
	}
	if !app.PollResultsProjectionValid(projection) {
		return unsupportedContent()
	}
	return extractedContent{body: projection.Question, kind: "poll_results", pollResults: &projection}
}

func projectedListContent(protocolList *waE2E.ListMessage) extractedContent {
	if protocolList == nil || protocolList.GetProductListInfo() != nil || len(protocolList.GetSections()) == 0 || len(protocolList.GetSections()) > app.ListSectionMaxCount {
		return unsupportedContent()
	}
	projection := app.ListProjection{
		Title:       protocolList.GetTitle(),
		Description: protocolList.GetDescription(),
		Footer:      protocolList.GetFooterText(),
		ButtonLabel: protocolList.GetButtonText(),
		Sections:    make([]app.ListSection, 0, len(protocolList.GetSections())),
	}
	actionRows := make([]app.ListActionRow, 0)
	for sectionIndex, section := range protocolList.GetSections() {
		if section == nil || len(section.GetRows()) == 0 || len(section.GetRows()) > app.ListRowsPerSectionMax {
			return unsupportedContent()
		}
		projectedSection := app.ListSection{Title: section.GetTitle(), Rows: make([]app.ListRow, 0, len(section.GetRows()))}
		for rowIndex, row := range section.GetRows() {
			if row == nil {
				return unsupportedContent()
			}
			projectedSection.Rows = append(projectedSection.Rows, app.ListRow{Title: row.GetTitle(), Description: row.GetDescription()})
			actionRows = append(actionRows, app.ListActionRow{SectionIndex: sectionIndex, RowIndex: rowIndex, RowID: row.GetRowID()})
		}
		projection.Sections = append(projection.Sections, projectedSection)
	}
	if !app.ListProjectionValid(projection) {
		return unsupportedContent()
	}
	if !app.ListActionRowsValid(projection, actionRows) {
		actionRows = nil
	}
	return extractedContent{body: projection.Title, kind: "list", list: &projection, listActionRows: actionRows}
}

func projectedButtonsContent(message *waE2E.ButtonsMessage) extractedContent {
	if message == nil || len(message.Buttons) == 0 || len(message.Buttons) > app.ButtonsMaxCount {
		return unsupportedContent()
	}
	projection := app.ButtonsProjection{
		Body: message.GetContentText(), Footer: message.GetFooterText(),
		Buttons: make([]string, 0, len(message.Buttons)),
	}
	switch header := message.Header.(type) {
	case nil:
	case *waE2E.ButtonsMessage_Text:
		if header == nil {
			return unsupportedContent()
		}
		projection.Header = header.Text
	default:
		return unsupportedContent()
	}
	if message.HeaderType != nil && *message.HeaderType != waE2E.ButtonsMessage_TEXT &&
		!(*message.HeaderType == waE2E.ButtonsMessage_EMPTY && message.Header == nil) {
		return unsupportedContent()
	}
	actionRows := make([]app.ButtonActionRow, 0, len(message.Buttons))
	actionsSupported := true
	for index, button := range message.Buttons {
		if button == nil || button.ButtonText == nil || button.ButtonText.DisplayText == nil {
			return unsupportedContent()
		}
		projection.Buttons = append(projection.Buttons, button.ButtonText.GetDisplayText())
		if button.Type == nil || button.GetType() != waE2E.ButtonsMessage_Button_RESPONSE {
			actionsSupported = false
			continue
		}
		actionRows = append(actionRows, app.ButtonActionRow{ButtonIndex: index, ButtonID: button.GetButtonID()})
	}
	if !app.ButtonsProjectionValid(projection) {
		return unsupportedContent()
	}
	if !actionsSupported || !app.ButtonActionRowsValid(projection, actionRows) {
		actionRows = nil
	}
	return extractedContent{body: projection.Body, kind: "buttons", buttons: &projection, buttonActionRows: actionRows}
}

func pollCreationMessage(message *waE2E.Message) *waE2E.PollCreationMessage {
	if message == nil {
		return nil
	}
	if poll := message.GetPollCreationMessage(); poll != nil {
		return poll
	}
	if poll := message.GetPollCreationMessageV2(); poll != nil {
		return poll
	}
	if poll := message.GetPollCreationMessageV3(); poll != nil {
		return poll
	}
	if wrapped := message.GetPollCreationMessageV4(); wrapped != nil && wrapped.GetMessage() != nil {
		if poll := pollCreationMessage(wrapped.GetMessage()); poll != nil {
			return poll
		}
	}
	if poll := message.GetPollCreationMessageV5(); poll != nil {
		return poll
	}
	return message.GetPollCreationMessageV6()
}

func messageContent(evt *events.Message) extractedContent {
	if evt == nil {
		return extractedContent{}
	}
	if evt.IsViewOnce || evt.IsViewOnceV2 || evt.IsViewOnceV2Extension {
		return extractedContent{body: "[View-once content unavailable]", kind: "unsupported"}
	}
	if evt.IsEphemeral {
		return extractedContent{body: "[Disappearing message not retained]", kind: "unsupported"}
	}
	m := evt.Message
	if m == nil {
		return extractedContent{}
	}
	// ButtonsMessage context and actions never enter the stored display projection.
	if m.GetButtonsMessage() != nil {
		return projectedButtonsContent(m.GetButtonsMessage())
	}
	var out extractedContent
	switch {
	case m.GetConversation() != "":
		out.body, out.kind = m.GetConversation(), "text"
	case m.GetExtendedTextMessage().GetText() != "":
		extended := m.GetExtendedTextMessage()
		out.body, out.kind = extended.GetText(), "text"
		if preview, ok := app.BuildLinkPreviewProjection(out.body, extended.GetMatchedText(), extended.GetTitle(), extended.GetDescription()); ok {
			out.linkPreview = preview
		}
	case m.GetImageMessage() != nil:
		v := m.GetImageMessage()
		out.body, out.kind = v.GetCaption(), "image"
		out.attachment, out.payload = mediaAttachment("image", "", v.GetMimetype(), v.GetFileLength(), v.GetFileSHA256(), 0, v.GetWidth(), v.GetHeight(), v)
	case m.GetStickerMessage() != nil:
		v := m.GetStickerMessage()
		out.kind = "sticker"
		out.attachment, out.payload = mediaAttachment("sticker", "", v.GetMimetype(), v.GetFileLength(), v.GetFileSHA256(), 0, v.GetWidth(), v.GetHeight(), v)
	case m.GetVideoMessage() != nil:
		v := m.GetVideoMessage()
		out.body, out.kind = v.GetCaption(), "video"
		out.attachment, out.payload = mediaAttachment("video", "", v.GetMimetype(), v.GetFileLength(), nilOrHash(v.GetFileSHA256()), v.GetSeconds(), v.GetWidth(), v.GetHeight(), v)
	case m.GetAudioMessage() != nil:
		v := m.GetAudioMessage()
		out.kind = "audio"
		out.attachment, out.payload = mediaAttachment("audio", "", v.GetMimetype(), v.GetFileLength(), v.GetFileSHA256(), v.GetSeconds(), 0, 0, v)
		out.attachment.IsVoiceNote = v.GetPTT()
	case m.GetDocumentMessage() != nil:
		v := m.GetDocumentMessage()
		out.body, out.kind = v.GetCaption(), "document"
		name := v.GetFileName()
		if name == "" {
			name = v.GetTitle()
		}
		if out.body == "" {
			out.body = name
		}
		out.attachment, out.payload = mediaAttachment("document", name, v.GetMimetype(), v.GetFileLength(), v.GetFileSHA256(), 0, v.GetThumbnailWidth(), v.GetThumbnailHeight(), v)
	case m.GetContactMessage() != nil:
		cards, err := app.ParseVCard([]byte(m.GetContactMessage().GetVcard()))
		if err != nil || len(cards) != 1 {
			out.body, out.kind = "[Unsupported contact card]", "unsupported"
		} else {
			out.kind, out.contacts = "contact", cards
		}
	case m.GetContactsArrayMessage() != nil:
		contacts := m.GetContactsArrayMessage().GetContacts()
		cards := make([]app.ContactCard, 0, len(contacts))
		totalBytes := 0
		valid := len(contacts) > 0 && len(contacts) <= app.ContactCardMaxCount
		for _, contact := range contacts {
			if !valid || contact == nil {
				valid = false
				break
			}
			totalBytes += len(contact.GetVcard())
			if totalBytes > app.ContactVCardMaxBytes {
				valid = false
				break
			}
			parsed, err := app.ParseVCard([]byte(contact.GetVcard()))
			if err != nil || len(parsed) != 1 {
				valid = false
				break
			}
			cards = append(cards, parsed[0])
		}
		if !valid {
			out.body, out.kind = "[Unsupported contact card]", "unsupported"
		} else {
			out.kind, out.contacts = "contact", cards
		}
	case m.GetLocationMessage() != nil:
		v := m.GetLocationMessage()
		if v.DegreesLatitude == nil || v.DegreesLongitude == nil {
			out = unsupportedLocationContent()
		} else {
			out = projectedLocationContent(app.LocationProjection{Latitude: v.GetDegreesLatitude(), Longitude: v.GetDegreesLongitude(), IsLive: v.GetIsLive(), Caption: v.GetComment(), Name: v.GetName(), Address: v.GetAddress()})
		}
	case m.GetLiveLocationMessage() != nil:
		v := m.GetLiveLocationMessage()
		if v.DegreesLatitude == nil || v.DegreesLongitude == nil {
			out = unsupportedLocationContent()
		} else {
			out = projectedLocationContent(app.LocationProjection{Latitude: v.GetDegreesLatitude(), Longitude: v.GetDegreesLongitude(), IsLive: true, Caption: v.GetCaption()})
		}
	case pollCreationMessage(m) != nil:
		out = projectedPollContent(pollCreationMessage(m))
	case m.GetPollResultSnapshotMessage() != nil:
		out = projectedPollResultsContent(m.GetPollResultSnapshotMessage())
	case m.GetListMessage() != nil:
		out = projectedListContent(m.GetListMessage())
	case m.GetInteractiveMessage() != nil:
		out = projectedInteractiveContent(m.GetInteractiveMessage())
	case m.GetTemplateMessage() != nil:
		out = projectedTemplateContent(m.GetTemplateMessage())
	case m.GetCallLogMesssage() != nil:
		out.kind, out.body = "call", callLogSummary(m.GetCallLogMesssage())
	case m.GetCall() != nil:
		out.kind = "call"
	case m.GetProtocolMessage() != nil || m.GetReactionMessage() != nil || m.GetPollUpdateMessage() != nil:
		return extractedContent{}
	default:
		out.body, out.kind = "[Unsupported content — open the official app]", "unsupported"
	}
	if info := messageContext(m); info != nil && info.GetQuotedMessage() != nil {
		out.quotedBody = quotedMessagePreview(info.GetQuotedMessage())
		out.quotedSender = info.GetParticipant()
	}
	return out
}

func quotedMessagePreview(message *waE2E.Message) string {
	if message == nil {
		return ""
	}
	if message.GetViewOnceMessage() != nil ||
		message.GetViewOnceMessageV2() != nil ||
		message.GetViewOnceMessageV2Extension() != nil {
		return "[View-once content unavailable]"
	}
	if message.GetEphemeralMessage() != nil {
		return "[Disappearing message not retained]"
	}
	if message.GetLocationMessage() != nil || message.GetLiveLocationMessage() != nil {
		return "Location"
	}

	content := messageContent(&events.Message{Message: message})
	if content.body != "" {
		return content.body
	}
	switch {
	case message.GetImageMessage() != nil:
		return "Photo"
	case message.GetStickerMessage() != nil:
		return "Sticker"
	case message.GetVideoMessage() != nil:
		return "Video"
	case message.GetAudioMessage() != nil:
		return "Audio"
	case message.GetLocationMessage() != nil || message.GetLiveLocationMessage() != nil:
		return "Location"
	default:
		return content.body
	}
}

func nilOrHash(hash []byte) []byte { return hash }
func firstText(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func messageContext(m *waE2E.Message) *waE2E.ContextInfo {
	if m == nil {
		return nil
	}
	if m.GetExtendedTextMessage() != nil {
		return m.GetExtendedTextMessage().GetContextInfo()
	}
	if m.GetImageMessage() != nil {
		return m.GetImageMessage().GetContextInfo()
	}
	if m.GetVideoMessage() != nil {
		return m.GetVideoMessage().GetContextInfo()
	}
	if m.GetAudioMessage() != nil {
		return m.GetAudioMessage().GetContextInfo()
	}
	if m.GetDocumentMessage() != nil {
		return m.GetDocumentMessage().GetContextInfo()
	}
	if m.GetStickerMessage() != nil {
		return m.GetStickerMessage().GetContextInfo()
	}
	if m.GetLocationMessage() != nil {
		return m.GetLocationMessage().GetContextInfo()
	}
	if m.GetLiveLocationMessage() != nil {
		return m.GetLiveLocationMessage().GetContextInfo()
	}
	if m.GetContactMessage() != nil {
		return m.GetContactMessage().GetContextInfo()
	}
	if m.GetContactsArrayMessage() != nil {
		return m.GetContactsArrayMessage().GetContextInfo()
	}
	if poll := pollCreationMessage(m); poll != nil {
		return poll.GetContextInfo()
	}
	if m.GetListMessage() != nil {
		return m.GetListMessage().GetContextInfo()
	}
	if m.GetButtonsMessage() != nil {
		return nil
	}
	if m.GetInteractiveMessage() != nil {
		return m.GetInteractiveMessage().GetContextInfo()
	}
	if m.GetTemplateMessage() != nil {
		return m.GetTemplateMessage().GetContextInfo()
	}
	return nil
}

func (a *Adapter) knownTitle(ctx context.Context, jid types.JID, fallback string) string {
	if ctx == nil || ctx.Err() != nil {
		return fallback
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || c.Store == nil || c.Store.Contacts == nil {
		return fallback
	}
	contact, err := c.Store.Contacts.GetContact(ctx, jid)
	if err != nil || !contact.Found {
		return fallback
	}
	return firstText(contact.FullName, contact.FirstName, contact.BusinessName, contact.PushName, fallback)
}

func (a *Adapter) record(ctx context.Context, evt *events.Message, title string) {
	a.recordWithOrigin(ctx, evt, title, false)
}

func (a *Adapter) recordWithOrigin(ctx context.Context, evt *events.Message, title string, liveInbound bool) bool {
	if evt == nil || !a.scopeAllowsMessage(evt.Info.Chat.String(), a.activeScopeClient(), evt.Info.Timestamp) {
		return false
	}
	content := messageContent(evt)
	if content.body == "" && content.kind == "" {
		return false
	}
	status := "received"
	if evt.Info.IsFromMe {
		status = "accepted"
	}
	var sender string
	if evt.Info.Sender != evt.Info.Chat {
		sender = evt.Info.PushName
	}
	var payload []byte
	if content.attachment != nil && !evt.IsViewOnce && !evt.IsEphemeral {
		payload = content.payload
	}
	var contactCards *[]app.ContactCard
	if len(content.contacts) > 0 {
		contactCards = &content.contacts
	}
	var listActionRows *[]app.ListActionRow
	if !evt.Info.IsFromMe && len(content.listActionRows) > 0 {
		rows := append([]app.ListActionRow(nil), content.listActionRows...)
		listActionRows = &rows
	}
	var buttonActionRows *[]app.ButtonActionRow
	if content.buttons != nil {
		rows := append([]app.ButtonActionRow(nil), content.buttonActionRows...)
		buttonActionRows = &rows
	}
	message := app.Message{ID: evt.Info.ID, ChatID: evt.Info.Chat.String(), Body: content.body, Timestamp: evt.Info.Timestamp.UnixMilli(), Outgoing: evt.Info.IsFromMe, Status: status, Kind: content.kind, SenderName: sender, SenderID: evt.Info.Sender.String(), QuotedBody: content.quotedBody, QuotedSender: content.quotedSender, Attachment: content.attachment, ContactCards: contactCards, Location: content.location, Poll: content.poll, PollResults: content.pollResults, List: content.list, ListActionRows: listActionRows, Buttons: content.buttons, ButtonActionRows: buttonActionRows, Interactive: content.interactive, Template: content.template, LinkPreview: content.linkPreview, MediaPayload: string(payload)}
	record := a.app.Record
	age := time.Since(evt.Info.Timestamp)
	if liveInbound && evt.SourceWebMsg == nil && evt.UnavailableRequestID == "" &&
		age >= -5*time.Second && age <= 30*time.Second {
		record = a.app.RecordLive
	}
	if e := record(ctx, message, title); e != nil {
		a.app.SetStatusContext(ctx, "error", "Incoming history could not be saved. Check disk and restart.", "")
		return false
	}
	a.scope.rememberAcceptedMessage(message.ChatID, message.ID)
	return true
}

func (a *Adapter) activeScopeClient() *whatsmeow.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *Adapter) scopeAllowsMutationTarget(chat, id string) bool {
	return a.scope == nil || a.scope.allowsMutationTarget(chat, id)
}

func (a *Adapter) event(ctx context.Context, c *whatsmeow.Client, event any) {
	switch e := event.(type) {
	case *events.Connected:
		a.app.SetStatusContext(ctx, "connected", "Connected · experimental preview", "")
	case *events.Disconnected:
		a.app.SetStatusContext(ctx, "disconnected", "WhatsApp disconnected; cached history is available", "")
	case *events.LoggedOut:
		a.app.SetStatusContext(ctx, "error", "Device unlinked by WhatsApp. Relinking requires explicit action.", "")
	case *events.Message:
		if e == nil || !a.scopeAllowsMessage(e.Info.Chat.String(), c, e.Info.Timestamp) {
			return
		}
		if e.Message != nil && e.Message.GetPollUpdateMessage() != nil {
			key := e.Message.GetPollUpdateMessage().GetPollCreationMessageKey()
			if key == nil || !a.scopeAllowsMutationTarget(e.Info.Chat.String(), key.GetID()) {
				return
			}
			a.receivePollVote(ctx, c, e)
			return
		}
		if e.Message != nil {
			if reaction := e.Message.GetReactionMessage(); reaction != nil && reaction.GetKey() != nil {
				key := reaction.GetKey()
				chat := key.GetRemoteJID()
				if chat == "" {
					chat = e.Info.Chat.String()
				}
				if !a.scopeAllowsChat(chat, c) || !a.scopeAllowsMutationTarget(chat, key.GetID()) {
					return
				}
				_ = a.app.ApplyIncomingReaction(ctx, chat, key.GetID(), e.Info.Sender.String(), reaction.GetText(), e.Info.Timestamp.UnixMilli())
				return
			}
			if protocol := e.Message.GetProtocolMessage(); protocol != nil && protocol.GetType() == waE2E.ProtocolMessage_REVOKE && protocol.GetKey() != nil {
				key := protocol.GetKey()
				chat := key.GetRemoteJID()
				if chat == "" {
					chat = e.Info.Chat.String()
				}
				if !a.scopeAllowsChat(chat, c) || !a.scopeAllowsMutationTarget(chat, key.GetID()) {
					return
				}
				actor := e.Info.Sender.String()
				if actor == "" {
					actor = key.GetParticipant()
				}
				_ = a.app.ApplyIncomingRevoke(ctx, chat, key.GetID(), actor)
				return
			}
			if e.IsEdit {
				if !a.scopeAllowsMutationTarget(e.Info.Chat.String(), e.Info.ID) {
					return
				}
				content := messageContent(e)
				_ = a.app.ApplyIncomingEdit(ctx, e.Info.Chat.String(), e.Info.ID, content.body, e.Info.Sender.String())
				return
			}
		}
		a.recordWithOrigin(ctx, e, a.knownTitle(ctx, e.Info.Chat, e.Info.PushName), true)
	case *events.HistorySync:
		for _, conv := range e.Data.GetConversations() {
			if ctx.Err() != nil {
				return
			}
			jid, err := types.ParseJID(conv.GetID())
			if err != nil || (a.scope != nil &&
				(jid.String() != conv.GetID() || !a.scopeAllowsChat(jid.String(), c))) {
				continue
			}
			for _, h := range conv.GetMessages() {
				if ctx.Err() != nil {
					return
				}
				webMessage := h.GetMessage()
				if webMessage == nil {
					continue
				}
				if a.scope != nil {
					messageSeconds := int64(webMessage.GetMessageTimestamp())
					if messageSeconds <= 0 || !a.scopeAllowsMessage(jid.String(), c, time.Unix(messageSeconds, 0)) {
						continue
					}
				}
				evt, err := c.ParseWebMessage(jid, webMessage)
				if err != nil {
					continue
				}
				if a.scope != nil && !a.scopeAllowsMessage(evt.Info.Chat.String(), c, evt.Info.Timestamp) {
					continue
				}
				if evt.Message != nil && evt.Message.GetPollUpdateMessage() != nil {
					a.receivePollVote(ctx, c, evt)
					continue
				}
				a.record(ctx, evt, a.knownTitle(ctx, jid, conv.GetName()))
			}
		}
	case *events.Receipt:
		status := ""
		if e.Type == types.ReceiptTypeRead {
			status = "read"
		} else if e.Type == types.ReceiptTypeDelivered {
			status = "delivered"
		}
		if status != "" {
			chat := e.Chat.String()
			if !a.scopeAllowsChat(chat, c) {
				return
			}
			ids := e.MessageIDs
			if a.scope != nil {
				ids = a.scope.acceptedReceiptIDs(chat, ids)
			}
			if len(ids) > 0 {
				a.app.Receipt(ctx, chat, ids, status)
			}
		}
	}
}
