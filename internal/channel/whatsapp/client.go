package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// waClient is the part of *whatsmeow.Client the adapter uses, plus Close for
// the session database under it. Tests fake it; realClient wraps whatsmeow.
type waClient interface {
	// Connect opens the websocket. It returns once the noise handshake is done;
	// Connected (or QR, on a fresh device) follows as an event.
	Connect(ctx context.Context) error
	Disconnect()
	// OwnID is the account's phone number JID and LID; pn is empty while the
	// device is not linked.
	OwnID() (pn, lid types.JID)
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
	// Logout unlinks the device on WhatsApp's side and deletes it locally.
	Logout(ctx context.Context) error
	// DeleteLocalDevice deletes the device from the local store only, with no
	// logout on WhatsApp's side. Used only when a link failed: there is no
	// linked device on WhatsApp to log out of. Unlink is the path that logs
	// out on WhatsApp's side.
	DeleteLocalDevice(ctx context.Context) error
	// AddEventHandler registers h for every event. Returning false withholds
	// the ack of the message being handled, so the server redelivers it.
	AddEventHandler(h func(evt any) bool)

	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	Upload(ctx context.Context, data []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	// DownloadToFile streams a message's media into file. The adapter hands it
	// a capped file, so a sender-declared length never decides memory or disk.
	DownloadToFile(ctx context.Context, msg whatsmeow.DownloadableMessage, file whatsmeow.File) error
	SendPresence(ctx context.Context, state types.Presence) error
	SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error
	MarkRead(ctx context.Context, ids []types.MessageID, ts time.Time, chat, sender types.JID, extra ...types.ReceiptType) error
	GenerateMessageID() types.MessageID
	// BuildReaction builds a reaction to message id in chat sent by sender
	// (empty for the account itself); an empty emoji takes the reaction off.
	BuildReaction(chat, sender types.JID, id types.MessageID, emoji string) *waE2E.Message
	// RejectCall declines an incoming call; it never answers one (§5.1 Calls).
	RejectCall(ctx context.Context, callFrom types.JID, callID string) error

	// Close disconnects and closes the session database.
	Close() error
}

// realClient is a whatsmeow client over its sqlstore container. openSession
// (store.go) is what builds one.
type realClient struct {
	cli       *whatsmeow.Client
	container *sqlstore.Container
}

var _ waClient = (*realClient)(nil)

func (r *realClient) Connect(ctx context.Context) error { return r.cli.ConnectContext(ctx) }
func (r *realClient) Disconnect()                       { r.cli.Disconnect() }

func (r *realClient) OwnID() (types.JID, types.JID) {
	var pn types.JID
	if r.cli.Store.ID != nil {
		pn = *r.cli.Store.ID
	}
	return pn, r.cli.Store.LID
}

func (r *realClient) PairPhone(ctx context.Context, phone string, show bool, ct whatsmeow.PairClientType, name string) (string, error) {
	return r.cli.PairPhone(ctx, phone, show, ct, name)
}

func (r *realClient) Logout(ctx context.Context) error            { return r.cli.Logout(ctx) }
func (r *realClient) DeleteLocalDevice(ctx context.Context) error { return r.cli.Store.Delete(ctx) }

func (r *realClient) AddEventHandler(h func(evt any) bool) {
	r.cli.AddEventHandlerWithSuccessStatus(h)
}

func (r *realClient) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return r.cli.SendMessage(ctx, to, msg, extra...)
}

func (r *realClient) Upload(ctx context.Context, data []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return r.cli.Upload(ctx, data, mt)
}

func (r *realClient) DownloadToFile(ctx context.Context, msg whatsmeow.DownloadableMessage, file whatsmeow.File) error {
	return r.cli.DownloadToFile(ctx, msg, file)
}

func (r *realClient) SendPresence(ctx context.Context, state types.Presence) error {
	return r.cli.SendPresence(ctx, state)
}

func (r *realClient) SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
	return r.cli.SendChatPresence(ctx, jid, state, media)
}

func (r *realClient) MarkRead(ctx context.Context, ids []types.MessageID, ts time.Time, chat, sender types.JID, extra ...types.ReceiptType) error {
	return r.cli.MarkRead(ctx, ids, ts, chat, sender, extra...)
}

func (r *realClient) GenerateMessageID() types.MessageID { return r.cli.GenerateMessageID() }

func (r *realClient) BuildReaction(chat, sender types.JID, id types.MessageID, emoji string) *waE2E.Message {
	return r.cli.BuildReaction(chat, sender, id, emoji)
}

func (r *realClient) RejectCall(ctx context.Context, callFrom types.JID, callID string) error {
	return r.cli.RejectCall(ctx, callFrom, callID)
}

func (r *realClient) Close() error {
	r.cli.Disconnect()
	return r.container.Close()
}

// logBridge sends whatsmeow's warnings and errors to slog and drops the rest:
// its debug level dumps decrypted stanzas (§5.1).
type logBridge struct {
	log    *slog.Logger
	module string
}

var _ waLog.Logger = logBridge{}

func newLogBridge(log *slog.Logger, module string) waLog.Logger {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return logBridge{log: log, module: module}
}

func (b logBridge) Warnf(msg string, args ...any) {
	if closedStoreDelete(msg, args) {
		return
	}
	b.log.Warn(fmt.Sprintf(msg, args...), "module", b.module)
}

// closedStoreDelete matches whatsmeow's own store delete after a logout
// failing because the adapter already closed the database: whatsmeow runs
// that delete in its node handler, which can outlive the socket, while the
// adapter wipes the whole file anyway. It is noise, not a failure.
func closedStoreDelete(msg string, args []any) bool {
	if !strings.HasPrefix(msg, "Failed to delete store after") {
		return false
	}
	for _, a := range args {
		// database/sql does not export its "database is closed" error.
		if err, ok := a.(error); ok && (errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "sql: database is closed")) {
			return true
		}
	}
	return false
}

func (b logBridge) Errorf(msg string, args ...any) {
	b.log.Error(fmt.Sprintf(msg, args...), "module", b.module)
}

func (b logBridge) Infof(string, ...any)  {}
func (b logBridge) Debugf(string, ...any) {}

func (b logBridge) Sub(module string) waLog.Logger {
	return logBridge{log: b.log, module: b.module + "/" + module}
}
