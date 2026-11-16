package whatsapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Bounds of the per-instance memory kept for read receipts.
const (
	maxUnreadPerDM = 64
	// maxConversations bounds the per-conversation maps (read receipts and
	// disappearing timers); the timers also live in the state store.
	maxConversations = 4096
	// maxAuthors bounds the remembered senders of messages, one per message.
	maxAuthors = 8192
	// maxEmojiBytes bounds the emoji of a reaction.
	maxEmojiBytes = 64
)

// handleMessage gets every event that is not a lifecycle one, from client
// generation gen. Its result is the ack: false withholds it, so the server
// redelivers and the decrypted event buffer replays the plaintext (§5.1).
func (a *Adapter) handleMessage(gen int, evt any) bool {
	a.stateMu.Lock()
	ctx, in, current := a.runCtx, a.inbound, gen == a.clientGen
	a.stateMu.Unlock()
	if ctx == nil || !current {
		return false
	}
	switch ev := evt.(type) {
	case *events.Message:
		return a.message(ctx, in, ev)
	case *events.HistorySync:
		// Dropped unread: never stored, never routed, never answered.
	case *events.JoinedGroup:
		a.joined(ctx, ev)
	case *events.CallOffer:
		a.call(ctx, gen, ev.BasicCallMeta)
	case *events.CallOfferNotice:
		a.call(ctx, gen, ev.BasicCallMeta)
	}
	return true
}

// message stores one message through Inbound and reports whether to ack it.
func (a *Adapter) message(ctx context.Context, in channel.Inbound, ev *events.Message) bool {
	info := ev.Info
	reason := dropReason(ev)
	if rm := ev.Message.GetReactionMessage(); reason == "" && rm != nil {
		switch {
		case !a.reactsToOwn(ev, rm.GetKey()):
			reason = "reaction on another's message"
		case rm.GetText() != "" && !validEmoji(rm.GetText()):
			reason = "reaction with a bad emoji"
		}
	}
	if reason != "" {
		a.log.Debug("whatsapp message dropped", "channel", a.name, "id", info.ID, "reason", reason)
		if p := ev.Message.GetProtocolMessage(); p.GetType() == waE2E.ProtocolMessage_EPHEMERAL_SETTING {
			a.setExpiration(ctx, conversationOf(info), p.GetEphemeralExpiration())
		}
		return true
	}
	conv := conversationOf(info)
	reaction := ev.Message.GetReactionMessage()
	var env event.Envelope
	if reaction != nil {
		env = a.reactionEnvelope(ev, conv, reaction)
	} else {
		env = a.envelope(ctx, ev, conv, conv+"/"+info.ID)
	}
	// Every copy goes to core, which drops a duplicate and records the keys
	// it carries that earlier copies did not.
	env.DedupeKeys = a.dedupeKeys(info)
	if in != nil {
		err := in(ctx, env)
		if errors.Is(err, channel.ErrDuplicate) {
			// Handled before: ack, and no read receipt.
			return true
		}
		if err != nil {
			a.log.Warn("whatsapp inbound not stored; WhatsApp will redeliver", "channel", a.name, "id", info.ID, "error", err)
			return false
		}
	}
	if reaction != nil {
		// No timer, read receipt or sender to remember: it is not a message.
		return true
	}
	if info.IsFromMe {
		a.noteAuthor(conv+"/"+info.ID, types.EmptyJID)
	} else {
		a.noteAuthor(conv+"/"+info.ID, info.Sender)
	}
	if ci := contextOf(ev.Message); ci != nil {
		a.setExpiration(ctx, conv, ci.GetExpiration())
	} else if !ev.IsEphemeral {
		a.setExpiration(ctx, conv, 0)
	}
	if !info.IsFromMe && (env.Origin.ConversationKind == event.ConversationDM || env.Mentioned) {
		var alt types.JID
		if info.Sender.Server == types.HiddenUserServer {
			alt = info.SenderAlt // trusted only here, as senderAliases explains
		}
		a.addUnread(conv, readRef{id: info.ID, chat: info.Chat, sender: info.Sender, alt: alt})
	}
	return true
}

// dropReason names why a message is not routed, or "" (§5.1 Dropped).
func dropReason(ev *events.Message) string {
	info, m := ev.Info, ev.Message
	switch {
	case info.Chat == types.StatusBroadcastJID:
		return "status"
	case info.Chat.Server == types.BroadcastServer:
		return "broadcast list"
	case info.Chat.Server == types.NewsletterServer:
		return "newsletter"
	case ev.IsEdit || info.Edit != "":
		return "edit"
	case m == nil:
		return "empty"
	case m.GetProtocolMessage() != nil:
		return "protocol"
	case m.GetEncReactionMessage() != nil:
		return "encrypted reaction"
	case m.GetReactionMessage() != nil:
		if m.GetReactionMessage().GetKey().GetID() == "" {
			return "reaction without a target"
		}
		return "" // routed as a reaction envelope
	case m.GetPollCreationMessage() != nil || m.GetPollCreationMessageV2() != nil ||
		m.GetPollCreationMessageV3() != nil || m.GetPollUpdateMessage() != nil:
		return "poll"
	}
	if text, _ := textOf(m); text == "" && mediaOf(m).kind == "" && stubOf(m) == "" {
		return "unsupported"
	}
	return ""
}

// envelope maps a routable message onto an Envelope.
func (a *Adapter) envelope(ctx context.Context, ev *events.Message, conv, key string) event.Envelope {
	info := ev.Info
	kind := event.ConversationDM
	if info.IsGroup {
		kind = event.ConversationRoom
	}
	text, ci := textOf(ev.Message)
	pn, lid := a.ownID()
	mentioned := kind == event.ConversationDM || mentions(ci, pn, lid)
	text = stripSelfMention(text, pn, lid)

	var notes []string
	var atts []event.Attachment
	switch {
	case ev.IsViewOnce && mediaOf(ev.Message).kind != "":
		notes = append(notes, "[view-once media, not shown]")
	case mediaOf(ev.Message).kind != "":
		if att, note, ok := a.attachment(ctx, ev.Message); ok {
			atts = append(atts, att)
		} else {
			notes = append(notes, note)
		}
	default:
		if s := stubOf(ev.Message); s != "" {
			notes = append(notes, s)
		}
	}
	if len(notes) > 0 {
		text = strings.TrimSpace(text + "\n" + strings.Join(notes, "\n"))
	}
	ts := info.Timestamp
	if ts.IsZero() {
		ts = a.now()
	}
	return event.Envelope{
		Schema: event.SchemaVersion,
		Origin: event.Origin{
			Channel: a.name, ChannelType: a.Type(),
			Conversation: conv, ConversationKind: kind, PlatformMsgID: key,
		},
		Sender:    event.Sender{Aliases: senderAliases(info), DisplayName: info.PushName, FromSelf: info.IsFromMe},
		Mentioned: mentioned,
		Message: event.Message{
			Schema: event.SchemaVersion, ID: event.NewID(), Role: event.RoleUser,
			Text: text, Attachments: atts, CreatedAt: ts.UTC(),
		},
		Received: a.now().UTC(),
	}
}

// reactionEnvelope maps a ReactionMessage onto an Envelope with a reaction.
// Its PlatformMsgID is the reaction's own id, tagged "r:" so it never collides
// with a message's, for dedupe; the target uses the id format of messages.
// An empty text means the reaction was taken off. Mentioned stays false on
// purpose: a reaction never addresses the bot in a group.
func (a *Adapter) reactionEnvelope(ev *events.Message, conv string, rm *waE2E.ReactionMessage) event.Envelope {
	info := ev.Info
	kind := event.ConversationDM
	if info.IsGroup {
		kind = event.ConversationRoom
	}
	ts := info.Timestamp
	if ts.IsZero() {
		ts = a.now()
	}
	emoji := rm.GetText()
	return event.Envelope{
		Schema: event.SchemaVersion,
		Origin: event.Origin{
			Channel: a.name, ChannelType: a.Type(),
			Conversation: conv, ConversationKind: kind, PlatformMsgID: conv + "/r:" + info.ID,
		},
		Sender:   event.Sender{Aliases: senderAliases(info), DisplayName: info.PushName, FromSelf: info.IsFromMe},
		Reaction: &event.Reaction{TargetMsgID: conv + "/" + rm.GetKey().GetID(), Emoji: emoji, Removed: emoji == ""},
		Message: event.Message{
			Schema: event.SchemaVersion, ID: event.NewID(), Role: event.RoleUser, CreatedAt: ts.UTC(),
		},
		Received: a.now().UTC(),
	}
}

// noteAuthor remembers who sent a message, up to maxAuthors messages. Message
// ids are chosen by their senders, so the first writer wins: a later message
// reusing an id cannot redirect a reaction. Only the account's own send
// (empty sender) may replace an entry, never the other way round.
func (a *Adapter) noteAuthor(key string, sender types.JID) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	old, ok := a.authors[key]
	if ok && (old.IsEmpty() || !sender.IsEmpty()) {
		return
	}
	if !ok {
		evictOldest(a.authors, &a.authorsOrder, key, maxAuthors)
	}
	a.authors[key] = sender
}

// reactsToOwn reports whether a reaction targets a message of the account
// itself. Message ids are per sender and chosen by it, so "chat/id" alone
// would let a group member's decoy message stand in for the bot's prompt;
// the key names the author. FromMe is from the reactor's view: a peer's
// reaction on the bot's message has FromMe false and, in a group, the bot as
// Participant.
func (a *Adapter) reactsToOwn(ev *events.Message, key *waCommon.MessageKey) bool {
	info := ev.Info
	if rj := key.GetRemoteJID(); rj != "" {
		j, err := types.ParseJID(rj)
		if err != nil || info.IsGroup != (j.Server == types.GroupServer) || info.IsGroup && j.ToNonAD() != info.Chat.ToNonAD() {
			return false
		}
	}
	if info.IsFromMe {
		return key.GetFromMe() // the account reacting on its own message
	}
	if key.GetFromMe() {
		return false // the reactor's own message
	}
	if !info.IsGroup {
		return true // two parties: not the reactor's, so ours
	}
	p, err := types.ParseJID(key.GetParticipant())
	if err != nil || p.User == "" {
		return false
	}
	pn, lid := a.ownID()
	p = p.ToNonAD()
	return p == pn.ToNonAD() && pn.User != "" || p == lid.ToNonAD() && lid.User != ""
}

// validEmoji accepts what an emoji is made of: symbols, marks, format
// characters (ZWJ, tags) and the digits, # and * of keycaps, in at most
// maxEmojiBytes. Text of a sender is never routed through a reaction.
func validEmoji(s string) bool {
	if s == "" || len(s) > maxEmojiBytes {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '#', r == '*':
		case unicode.IsSymbol(r), unicode.IsMark(r), unicode.Is(unicode.Cf, r):
		default:
			return false
		}
	}
	return true
}

// conversationOf is the conversation id: the group JID, or in a DM the peer's
// phone number JID when the stanza names it, else its LID (§4).
func conversationOf(info types.MessageInfo) string {
	chat := info.Chat.ToNonAD()
	if info.IsGroup || chat.Server != types.HiddenUserServer {
		return chat.String()
	}
	// Only stanza attributes: RecipientAlt is peer_recipient_pn, and
	// SenderAlt counts only for a LID-addressed sender (see senderAliases).
	alt := []types.JID{info.Sender}
	if info.Sender.Server == types.HiddenUserServer {
		alt = append(alt, info.SenderAlt)
	}
	if info.IsFromMe {
		alt = []types.JID{info.RecipientAlt}
	}
	for _, j := range alt {
		if j.Server == types.DefaultUserServer {
			return j.ToNonAD().String()
		}
	}
	return chat.String()
}

// aliases are the sender ids of this stanza only, never whatsmeow's LID map:
// two ids in one stanza are one account, anything else is a guess (§4).
func aliases(ids ...types.JID) []string {
	var out []string
	for _, j := range ids {
		j = j.ToNonAD()
		var a string
		switch j.Server {
		case types.DefaultUserServer:
			a = "wa:+" + j.User
		case types.HiddenUserServer:
			a = "wa:lid:" + j.User
		}
		if a != "" && j.User != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// senderAliases are the aliases of a message's sender. SenderAlt is trusted
// only when the sender is LID-addressed: then it comes from the stanza's
// sender_pn or participant_pn. For a phone-addressed sender whatsmeow fills
// it from its own LID map, learned from earlier stanzas (§4).
func senderAliases(info types.MessageInfo) []string {
	if info.Sender.Server == types.HiddenUserServer {
		return aliases(info.Sender, info.SenderAlt)
	}
	return aliases(info.Sender)
}

// textOf returns the text or caption of m and its context info.
func textOf(m *waE2E.Message) (string, *waE2E.ContextInfo) {
	switch {
	case m.GetConversation() != "":
		return m.GetConversation(), nil
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption(), m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption(), m.GetVideoMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption(), m.GetDocumentMessage().GetContextInfo()
	}
	return "", contextOf(m)
}

// contextOf finds the context info of whichever part m carries.
func contextOf(m *waE2E.Message) *waE2E.ContextInfo {
	for _, ci := range []*waE2E.ContextInfo{
		m.GetExtendedTextMessage().GetContextInfo(), m.GetImageMessage().GetContextInfo(),
		m.GetVideoMessage().GetContextInfo(), m.GetAudioMessage().GetContextInfo(),
		m.GetDocumentMessage().GetContextInfo(), m.GetStickerMessage().GetContextInfo(),
		m.GetLocationMessage().GetContextInfo(), m.GetContactMessage().GetContextInfo(),
		m.GetContactsArrayMessage().GetContextInfo(), m.GetLiveLocationMessage().GetContextInfo(),
	} {
		if ci != nil {
			return ci
		}
	}
	return nil
}

// mentions reports whether ci names the bot: in MentionedJID, or as the
// author of a quoted message.
func mentions(ci *waE2E.ContextInfo, pn, lid types.JID) bool {
	if ci == nil {
		return false
	}
	isBot := func(s string) bool {
		j, err := types.ParseJID(s)
		if err != nil {
			return false
		}
		j = j.ToNonAD()
		return j.User != "" && (j == pn.ToNonAD() || j == lid.ToNonAD())
	}
	for _, s := range ci.GetMentionedJID() {
		if isBot(s) {
			return true
		}
	}
	return ci.GetStanzaID() != "" && isBot(ci.GetParticipant())
}

// stripSelfMention removes the bot's own @<number> tokens, so a group code
// sent as "@bot <code>" reaches the gate bare.
func stripSelfMention(text string, pn, lid types.JID) string {
	for _, u := range []string{pn.User, lid.User} {
		if u == "" {
			continue
		}
		tok := "@" + u
		for from := 0; ; {
			i := strings.Index(text[from:], tok)
			if i < 0 {
				break
			}
			i += from
			end := i + len(tok)
			if end < len(text) && text[end] >= '0' && text[end] <= '9' {
				from = end // a longer number, not this one
				continue
			}
			if end < len(text) && text[end] == ' ' {
				end++
			}
			text, from = text[:i]+text[end:], i
		}
	}
	return strings.TrimSpace(text)
}

// media is the downloadable part of a message, normalised.
type media struct {
	kind string // "" when there is none
	msg  whatsmeow.DownloadableMessage
	name string
	mime string
	size uint64
}

// mediaOf picks the media part of m: image, video, audio (voice notes
// included), document or sticker.
func mediaOf(m *waE2E.Message) media {
	switch {
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		return media{"image", x, "photo.jpg", cmp.Or(x.GetMimetype(), "image/jpeg"), x.GetFileLength()}
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		return media{"video", x, "video.mp4", cmp.Or(x.GetMimetype(), "video/mp4"), x.GetFileLength()}
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		name := "audio"
		if x.GetPTT() {
			name = "voice.ogg"
		}
		return media{"audio", x, name, cmp.Or(x.GetMimetype(), "audio/ogg; codecs=opus"), x.GetFileLength()}
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		return media{"document", x, cmp.Or(x.GetFileName(), "document"), x.GetMimetype(), x.GetFileLength()}
	case m.GetStickerMessage() != nil:
		x := m.GetStickerMessage()
		return media{"sticker", x, "sticker.webp", cmp.Or(x.GetMimetype(), "image/webp"), x.GetFileLength()}
	}
	return media{}
}

// stubOf is the one-line text for content holaclaw does not download.
func stubOf(m *waE2E.Message) string {
	switch {
	case m.GetLocationMessage() != nil:
		x := m.GetLocationMessage()
		return strings.TrimSpace(fmt.Sprintf("[location: %.5f, %.5f %s]", x.GetDegreesLatitude(), x.GetDegreesLongitude(), oneLine(x.GetName())))
	case m.GetLiveLocationMessage() != nil:
		x := m.GetLiveLocationMessage()
		return fmt.Sprintf("[live location: %.5f, %.5f]", x.GetDegreesLatitude(), x.GetDegreesLongitude())
	case m.GetContactMessage() != nil:
		return "[contact card: " + oneLine(m.GetContactMessage().GetDisplayName()) + "]"
	case m.GetContactsArrayMessage() != nil:
		return fmt.Sprintf("[%d contact cards]", len(m.GetContactsArrayMessage().GetContacts()))
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// attachment downloads a message's media into a blob. When the file cannot be
// delivered it returns a note for the text instead, as Telegram does.
func (a *Adapter) attachment(ctx context.Context, m *waE2E.Message) (event.Attachment, string, bool) {
	md := mediaOf(m)
	name := sanitiseName(md.name)
	if name == "" {
		name = md.kind
	}
	if md.size > uint64(a.maxDownload) {
		return event.Attachment{}, "[file too large: " + name + "]", false
	}
	cli := a.currentClient()
	if cli == nil || a.putBlob == nil {
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	// The declared length is the sender's word, so the body streams into a
	// temporary file next to the session (0700) that refuses to grow past
	// the cap, never into memory.
	tmp, err := os.CreateTemp(filepath.Dir(a.path), "media-*")
	if err != nil {
		a.log.Warn("whatsapp download failed", "channel", a.name, "error", err)
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	// Encrypted media carries up to a block of padding and a MAC on top.
	err = cli.DownloadToFile(ctx, md.msg, &cappedFile{f: tmp, limit: a.maxDownload + 64})
	if errors.Is(err, errTooLarge) {
		return event.Attachment{}, "[file too large: " + name + "]", false
	}
	if err != nil {
		a.log.Warn("whatsapp download failed", "channel", a.name, "error", err)
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	st, err := tmp.Stat()
	if err != nil {
		a.log.Warn("whatsapp download failed", "channel", a.name, "error", err)
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	n := st.Size()
	if n > a.maxDownload {
		return event.Attachment{}, "[file too large: " + name + "]", false
	}
	mimeType := md.mime
	if mimeType == "" {
		head := make([]byte, 512)
		k, _ := tmp.ReadAt(head, 0)
		mimeType = http.DetectContentType(head[:k])
	}
	blobID, size, err := a.putBlob(ctx, io.NewSectionReader(tmp, 0, n), mimeType)
	if err != nil {
		a.log.Warn("whatsapp blob store failed", "channel", a.name, "error", err)
		return event.Attachment{}, "[file unavailable: " + name + "]", false
	}
	if size == 0 {
		size = n
	}
	return event.Attachment{
		ID: event.NewID(), BlobID: blobID, MIME: mimeType, Name: name, Size: size,
		SHA256: blobID, // blob ids are the content address: hex sha256
		Kind:   event.KindFromMIME(mimeType),
	}, "", true
}

// errTooLarge is what cappedFile answers a write past its limit with.
var errTooLarge = errors.New("whatsapp: file over the download cap")

// cappedFile is a whatsmeow.File that fails any write or truncate past limit.
// It wraps the file instead of embedding it, so io.Copy cannot reach the
// file's own ReadFrom and bypass the cap.
type cappedFile struct {
	f     *os.File
	limit int64
}

var _ whatsmeow.File = (*cappedFile)(nil)

func (c *cappedFile) Write(p []byte) (int, error) {
	off, err := c.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if off+int64(len(p)) > c.limit {
		return 0, errTooLarge
	}
	return c.f.Write(p)
}

func (c *cappedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > c.limit {
		return 0, errTooLarge
	}
	return c.f.WriteAt(p, off)
}

func (c *cappedFile) Truncate(size int64) error {
	if size > c.limit {
		return errTooLarge
	}
	return c.f.Truncate(size)
}

func (c *cappedFile) Read(p []byte) (int, error)                { return c.f.Read(p) }
func (c *cappedFile) ReadAt(p []byte, off int64) (int, error)   { return c.f.ReadAt(p, off) }
func (c *cappedFile) Seek(off int64, whence int) (int64, error) { return c.f.Seek(off, whence) }
func (c *cappedFile) Stat() (os.FileInfo, error)                { return c.f.Stat() }

// sanitiseName reduces a sender-supplied file name to a safe base name:
// no path, no control or bidirectional-override characters, at most 120
// bytes cut on a rune boundary.
func sanitiseName(s string) string {
	s = path.Base(strings.ReplaceAll(s, "\\", "/"))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || unicode.IsSpace(r) && r != ' ' || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(strings.Trim(s, "."))
	for len(s) > 120 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// joined raises OnJoined for a group the account was added to, with the
// adder's ids when the event names them.
func (a *Adapter) joined(ctx context.Context, ev *events.JoinedGroup) {
	if a.onJoined == nil || ev.JID.IsEmpty() {
		return
	}
	var by []types.JID
	if ev.Sender != nil {
		by = append(by, *ev.Sender)
	}
	if ev.SenderPN != nil {
		by = append(by, *ev.SenderPN)
	}
	a.onJoined(ctx, ev.JID.ToNonAD().String(), aliases(by...))
}

// dedupeKeys are the keys core remembers a message under
// (Envelope.DedupeKeys): each id of its sender with the message id, in a
// group also the group. Ids are unique per sender, not per conversation, and
// WhatsApp may deliver one message under both addressings (a retry comes
// LID-addressed after a phone-addressed copy), so the conversation is never
// part of a key. SenderAlt counts here even when whatsmeow filled it from its
// LID map: a key only matches copies, it names no one (§4 forbids the map for
// aliases only). With no sender id there are no keys, and core falls back on
// PlatformMsgID, which keeps the conversation.
func (a *Adapter) dedupeKeys(info types.MessageInfo) []string {
	ids := []types.JID{info.Sender, info.SenderAlt}
	if info.IsFromMe {
		pn, lid := a.ownID()
		ids = append(ids, pn, lid)
	}
	prefix := ""
	if info.IsGroup {
		prefix = info.Chat.ToNonAD().String() + "/"
	}
	var keys []string
	for _, j := range ids {
		if j = j.ToNonAD(); j.User == "" {
			continue
		}
		if k := prefix + j.String() + "/" + info.ID; !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// readRef is a stored message that may start a turn, kept so the turn can
// mark it read. alt is the sender's phone-number alias, known only when
// sender is LID-addressed; it lets a known peer be recognised by either id.
type readRef struct {
	id     types.MessageID
	chat   types.JID
	sender types.JID
	alt    types.JID
}

func (a *Adapter) addUnread(conv string, r readRef) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	q, ok := a.unread[conv]
	if !ok {
		evictOldest(a.unread, &a.unreadOrder, conv, maxConversations)
	}
	q = append(q, r)
	if len(q) > maxUnreadPerDM {
		q = q[len(q)-maxUnreadPerDM:]
	}
	a.unread[conv] = q
}

// evictOldest makes room for a new key in a per-conversation map, dropping
// the oldest one past maxConversations, so strangers writing from many
// numbers cannot grow it without bound. The caller holds mu.
func evictOldest[V any](m map[string]V, order *[]string, key string, limit int) {
	// Keys already gone from m (taken, or evicted) are skipped on the way.
	for len(m) >= limit && len(*order) > 0 {
		delete(m, (*order)[0])
		*order = (*order)[1:]
	}
	if len(*order) >= 2*limit {
		live := (*order)[:0]
		for _, k := range *order {
			if _, ok := m[k]; ok {
				live = append(live, k)
			}
		}
		*order = live
	}
	*order = append(*order, key)
}

func (a *Adapter) takeUnread(conv string) []readRef {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	q := a.unread[conv]
	delete(a.unread, conv)
	return q
}

// expirationKey is where a conversation's disappearing timer lives in the
// instance state store, so a restart keeps honouring it.
func expirationKey(conv string) string { return "expiration:" + conv }

// setExpiration records the disappearing timer of a conversation (seconds,
// 0 = off).
func (a *Adapter) setExpiration(ctx context.Context, conv string, secs uint32) {
	a.stateMu.Lock()
	old, known := a.expiry[conv]
	if !known {
		evictOldest(a.expiry, &a.expiryOrder, conv, maxConversations)
	}
	a.expiry[conv] = secs
	a.stateMu.Unlock()
	if (known && old == secs) || a.state == nil {
		return
	}
	if err := a.state.Set(ctx, expirationKey(conv), strconv.FormatUint(uint64(secs), 10)); err != nil {
		a.log.Warn("whatsapp expiration not saved", "channel", a.name, "error", err)
	}
}

// expiration is the disappearing timer to copy onto a reply.
func (a *Adapter) expiration(ctx context.Context, conv string) uint32 {
	a.stateMu.Lock()
	secs, ok := a.expiry[conv]
	a.stateMu.Unlock()
	if ok || a.state == nil {
		return secs
	}
	v, found, err := a.state.Get(ctx, expirationKey(conv))
	if err != nil || !found {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0
	}
	a.stateMu.Lock()
	if _, known := a.expiry[conv]; !known {
		evictOldest(a.expiry, &a.expiryOrder, conv, maxConversations)
	}
	a.expiry[conv] = uint32(n)
	a.stateMu.Unlock()
	return uint32(n)
}

// currentClient is the current client, nil while not running.
func (a *Adapter) currentClient() waClient {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.client
}

// ownID is the account's phone number JID and LID.
func (a *Adapter) ownID() (types.JID, types.JID) {
	if cli := a.currentClient(); cli != nil {
		return cli.OwnID()
	}
	return types.JID{}, types.JID{}
}
