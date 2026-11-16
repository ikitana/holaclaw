package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake Telegram Bot API, small enough to read in one sitting and complete
// enough to drive the whole channel path of the daemon: long-poll getUpdates
// served from a queue tests push into, sendMessage / sendChatAction, file
// download, the upload methods, and the callback-query round trip.
//
// The telegram package has a fake of its own, but it lives in a _test file of
// that package and so cannot be imported here (tools/eval/README.md: tests use
// httptest servers, never the network).

// tgToken is the bot token the daemon is configured with. It is distinctive so
// a test can assert it never leaks, and so a wrong token is obviously wrong.
const tgToken = "424242:AAF-daemon-e2e-fake-bot-token"

// tgPollBudget is how long one getUpdates hangs before answering "nothing".
// The adapter's own long-poll timeout is 50 s (not a config key), so the fake
// is what keeps a test's poll loop short.
const tgPollBudget = 300 * time.Millisecond

// --- wire types ----------------------------------------------------------------

type tgUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type tgFileRef struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type tgMessage struct {
	MessageID       int64      `json:"message_id"`
	MessageThreadID int64      `json:"message_thread_id,omitempty"`
	IsTopicMessage  bool       `json:"is_topic_message,omitempty"`
	From            *tgUser    `json:"from,omitempty"`
	Chat            tgChat     `json:"chat"`
	Date            int64      `json:"date"`
	Text            string     `json:"text,omitempty"`
	Entities        []tgEntity `json:"entities,omitempty"`
	Voice           *tgFileRef `json:"voice,omitempty"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message,omitempty"`
	Data    string     `json:"data"`
}

type tgChatMember struct {
	User   *tgUser `json:"user"`
	Status string  `json:"status"`
}

type tgChatMemberUpdated struct {
	Chat          tgChat       `json:"chat"`
	From          *tgUser      `json:"from"`
	OldChatMember tgChatMember `json:"old_chat_member"`
	NewChatMember tgChatMember `json:"new_chat_member"`
}

type tgUpdate struct {
	UpdateID      int64                `json:"update_id"`
	Message       *tgMessage           `json:"message,omitempty"`
	CallbackQuery *tgCallback          `json:"callback_query,omitempty"`
	MyChatMember  *tgChatMemberUpdated `json:"my_chat_member,omitempty"`
}

// --- recorded calls ------------------------------------------------------------

// tgCall is one request the fake served.
type tgCall struct {
	Method   string
	Body     map[string]any    // JSON calls
	Fields   map[string]string // multipart calls
	FileName string
	File     []byte
}

// str reads a string field from either encoding.
func (c tgCall) str(key string) string {
	if v, ok := c.Fields[key]; ok {
		return v
	}
	if v, ok := c.Body[key].(string); ok {
		return v
	}
	return ""
}

// --- the server ----------------------------------------------------------------

type fakeTG struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    []tgCall
	queue    []tgUpdate        // updates not yet confirmed by an offset
	offsets  []int64           // the offset of every getUpdates request, in order
	files    map[string][]byte // bot-API file path → bytes
	paths    map[string]string // file id → bot-API file path
	nextUpd  int64
	nextMsg  int64
	botUser  string
	botID    int64
	getMeErr int // when > 0, getMe answers this status
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{
		t: t, files: map[string][]byte{}, paths: map[string]string{},
		nextUpd: 1000, nextMsg: 500, botUser: "holaclaw", botID: 99,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is what Options.ChannelBaseURL is pointed at.
func (f *fakeTG) URL() string { return f.srv.URL }

// mention is the text of a message that addresses the bot, with the entity a
// real client would attach.
func (f *fakeTG) mention(text string) (string, []tgEntity) {
	at := "@" + f.botUser
	return at + " " + text, []tgEntity{{Type: "mention", Offset: 0, Length: len(at)}}
}

// --- pushing updates -----------------------------------------------------------

// push queues one update and returns its id.
func (f *fakeTG) push(u tgUpdate) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUpd++
	u.UpdateID = f.nextUpd
	if u.Message != nil {
		f.nextMsg++
		u.Message.MessageID = f.nextMsg
		if u.Message.Date == 0 {
			u.Message.Date = time.Now().Unix()
		}
	}
	f.queue = append(f.queue, u)
	return u.UpdateID
}

// lastMsgID is the last message id handed out, to a pushed message or to
// one the bot sent.
func (f *fakeTG) lastMsgID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextMsg
}

// pushDM queues a private message from one sender.
func (f *fakeTG) pushDM(from tgUser, chatID int64, text string) int64 {
	return f.push(tgUpdate{Message: &tgMessage{
		From: &from, Chat: tgChat{ID: chatID, Type: "private"}, Text: text,
	}})
}

// pushGroup queues a group message; mentioned adds the bot mention entity.
func (f *fakeTG) pushGroup(from tgUser, chatID int64, text string, mentioned bool) int64 {
	m := &tgMessage{From: &from, Chat: tgChat{ID: chatID, Type: "supergroup"}, Text: text}
	if mentioned {
		m.Text, m.Entities = f.mention(text)
	}
	return f.push(tgUpdate{Message: m})
}

// pushJoined queues the bot being added to a group by one user.
func (f *fakeTG) pushJoined(by tgUser, chatID int64) int64 {
	f.mu.Lock()
	bot := tgUser{ID: f.botID, IsBot: true, Username: f.botUser}
	f.mu.Unlock()
	return f.push(tgUpdate{MyChatMember: &tgChatMemberUpdated{
		Chat: tgChat{ID: chatID, Type: "supergroup"}, From: &by,
		OldChatMember: tgChatMember{User: &bot, Status: "left"},
		NewChatMember: tgChatMember{User: &bot, Status: "member"},
	}})
}

// pushCallback queues a button press.
func (f *fakeTG) pushCallback(from tgUser, chatID int64, data string) int64 {
	return f.push(tgUpdate{CallbackQuery: &tgCallback{
		ID:   fmt.Sprintf("cb-%d", time.Now().UnixNano()),
		From: &from,
		Data: data,
		Message: &tgMessage{
			MessageID: 1, Chat: tgChat{ID: chatID, Type: "private"}, Text: "Allow: files.write?",
		},
	}})
}

// serveFile registers downloadable bytes behind a file id.
func (f *fakeTG) serveFile(fileID, path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths[fileID] = path
	f.files[path] = data
}

// --- inspection ----------------------------------------------------------------

func (f *fakeTG) seen(method string) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tgCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTG) count(method string) int { return len(f.seen(method)) }

// texts returns the text of every sendMessage, in order.
func (f *fakeTG) texts() []string {
	var out []string
	for _, c := range f.seen("sendMessage") {
		out = append(out, c.str("text"))
	}
	return out
}

// sent reports whether any sendMessage text contains want.
func (f *fakeTG) sent(want string) bool {
	for _, s := range f.texts() {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// lastOffset is the offset of the most recent getUpdates request.
func (f *fakeTG) lastOffset() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offsets) == 0 {
		return -1
	}
	return f.offsets[len(f.offsets)-1]
}

// --- HTTP ----------------------------------------------------------------------

func (f *fakeTG) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if rest, ok := strings.CutPrefix(path, "file/bot"); ok {
		f.serveDownload(w, rest)
		return
	}
	if !strings.HasPrefix(path, "bot"+tgToken+"/") {
		// A wrong or revoked token: exactly what the Bot API answers.
		writeTGError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	c := tgCall{Method: strings.TrimPrefix(path, "bot"+tgToken+"/")}
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		c.Fields = map[string]string{}
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				c.Fields[k] = v[0]
			}
			for _, fhs := range r.MultipartForm.File {
				fh := fhs[0]
				c.FileName = fh.Filename
				if fd, err := fh.Open(); err == nil {
					c.File, _ = io.ReadAll(fd)
					_ = fd.Close()
				}
			}
		}
	} else if body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20)); len(body) > 0 {
		_ = json.Unmarshal(body, &c.Body)
	}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()

	switch c.Method {
	case "getMe":
		f.mu.Lock()
		status, id, user := f.getMeErr, f.botID, f.botUser
		f.mu.Unlock()
		if status > 0 {
			writeTGError(w, status, "Unauthorized")
			return
		}
		writeTGOK(w, tgUser{ID: id, IsBot: true, Username: user})
	case "getUpdates":
		f.serveUpdates(w, r, c)
	case "getFile":
		f.mu.Lock()
		p := f.paths[c.str("file_id")]
		n := int64(len(f.files[p]))
		f.mu.Unlock()
		if p == "" {
			writeTGError(w, http.StatusBadRequest, "file not found")
			return
		}
		writeTGOK(w, map[string]any{"file_id": c.str("file_id"), "file_path": p, "file_size": n})
	case "sendMessage", "sendDocument", "sendPhoto", "sendVoice", "sendAudio", "sendVideo":
		f.mu.Lock()
		f.nextMsg++
		id := f.nextMsg
		f.mu.Unlock()
		writeTGOK(w, map[string]any{"message_id": id})
	default:
		// sendChatAction, answerCallbackQuery, editMessage*: `true`.
		writeTGOK(w, true)
	}
}

// serveUpdates is the long poll: it confirms everything below the offset, then
// answers with what is left as soon as there is anything.
func (f *fakeTG) serveUpdates(w http.ResponseWriter, r *http.Request, c tgCall) {
	offset := int64(0)
	if v, ok := c.Body["offset"].(float64); ok {
		offset = int64(v)
	}
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)
	kept := f.queue[:0]
	for _, u := range f.queue {
		if u.UpdateID >= offset {
			kept = append(kept, u)
		}
	}
	f.queue = kept
	f.mu.Unlock()

	deadline := time.Now().Add(tgPollBudget)
	for {
		f.mu.Lock()
		out := append([]tgUpdate(nil), f.queue...)
		f.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) || r.Context().Err() != nil {
			writeTGOK(w, out)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fakeTG) serveDownload(w http.ResponseWriter, rest string) {
	i := strings.IndexByte(rest, '/')
	if i < 0 || rest[:i] != tgToken {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	data, ok := f.files[rest[i+1:]]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, _ = w.Write(data)
}

func writeTGOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func writeTGError(w http.ResponseWriter, status int, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": false, "error_code": status, "description": desc,
	})
}
