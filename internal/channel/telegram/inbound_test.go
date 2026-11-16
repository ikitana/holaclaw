package telegram

import (
	"context"
	"strings"
	"testing"

	"holaclaw/internal/event"
)

// groupMsg builds a group message with text and entities.
func groupMsg(text string, ents ...wireEntity) *wireMessage {
	return &wireMessage{
		MessageID: 5,
		From:      &wireUser{ID: 7, Username: "Angel", FirstName: "Angel", LastName: "M"},
		Chat:      wireChat{ID: -100777, Type: "supergroup"},
		Date:      1700000000,
		Text:      text,
		Entities:  ents,
	}
}

func TestEnvelopeConversationMapping(t *testing.T) {
	f := newFake(t)
	a, _, _ := newAdapter(t, f, nil)
	a.setBot(99, "holaclaw")

	t.Run("dm", func(t *testing.T) {
		env, ok := a.envelope(t.Context(), []*wireMessage{dmMessage(12, "hi")})
		if !ok {
			t.Fatal("dropped")
		}
		if env.Origin.ConversationKind != event.ConversationDM {
			t.Fatalf("kind = %q", env.Origin.ConversationKind)
		}
		if env.Origin.Conversation != "1234" || env.Origin.PlatformMsgID != "1234:12" {
			t.Fatalf("origin = %+v", env.Origin)
		}
		if env.Origin.Channel != "tg-test" || env.Origin.ChannelType != "telegram" {
			t.Fatalf("origin = %+v", env.Origin)
		}
		if !env.Mentioned {
			t.Fatal("a DM is always addressed to the bot")
		}
		if env.Origin.Thread != "" {
			t.Fatalf("thread = %q", env.Origin.Thread)
		}
		// The username is no alias: someone else can claim it later.
		want := []string{"tg:7"}
		if strings.Join(env.Sender.Aliases, ",") != strings.Join(want, ",") {
			t.Fatalf("aliases = %v", env.Sender.Aliases)
		}
		if env.Sender.DisplayName != "Angel" {
			t.Fatalf("display name = %q", env.Sender.DisplayName)
		}
		if env.Message.Role != event.RoleUser || env.Message.Text != "hi" {
			t.Fatalf("message = %+v", env.Message)
		}
		if env.Message.ReplyTo != "" {
			t.Fatal("reply_to must stay empty: platform ids are not holaclaw ids")
		}
	})

	t.Run("group", func(t *testing.T) {
		env, _ := a.envelope(t.Context(), []*wireMessage{groupMsg("plain talk")})
		if env.Origin.ConversationKind != event.ConversationRoom {
			t.Fatalf("kind = %q", env.Origin.ConversationKind)
		}
		if env.Mentioned {
			t.Fatal("unmentioned group message must not be flagged")
		}
	})

	t.Run("forum topic", func(t *testing.T) {
		m := groupMsg("in a topic")
		m.IsTopicMessage, m.MessageThreadID = true, 42
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if env.Origin.Thread != "42" {
			t.Fatalf("thread = %q", env.Origin.Thread)
		}
	})

	t.Run("non forum thread id ignored", func(t *testing.T) {
		m := groupMsg("a reply")
		m.MessageThreadID = 42
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if env.Origin.Thread != "" {
			t.Fatalf("thread = %q", env.Origin.Thread)
		}
	})

	t.Run("from self is dropped", func(t *testing.T) {
		m := dmMessage(1, "echo")
		m.From = &wireUser{ID: 99, Username: "holaclaw"}
		if _, ok := a.envelope(t.Context(), []*wireMessage{m}); ok {
			t.Fatal("the bot's own message must not be routed")
		}
	})
}

func TestMentionDetection(t *testing.T) {
	f := newFake(t)
	a, _, _ := newAdapter(t, f, nil)
	a.setBot(99, "holaclaw")

	cases := []struct {
		name      string
		msg       *wireMessage
		mentioned bool
		text      string
	}{
		{
			name:      "entity mention at start is stripped",
			msg:       groupMsg("@holaclaw what is up", wireEntity{Type: "mention", Offset: 0, Length: 9}),
			mentioned: true,
			text:      "what is up",
		},
		{
			name:      "entity mention mid text is kept",
			msg:       groupMsg("hey @holaclaw now", wireEntity{Type: "mention", Offset: 4, Length: 9}),
			mentioned: true,
			text:      "hey @holaclaw now",
		},
		{
			name:      "mention of someone else",
			msg:       groupMsg("hey @someone", wireEntity{Type: "mention", Offset: 4, Length: 8}),
			mentioned: false,
			text:      "hey @someone",
		},
		{
			name: "text_mention of the bot",
			msg: groupMsg("look here", wireEntity{
				Type: "text_mention", Offset: 0, Length: 4, User: &wireUser{ID: 99},
			}),
			mentioned: true,
			text:      "look here",
		},
		{
			name: "text_mention of another user",
			msg: groupMsg("look here", wireEntity{
				Type: "text_mention", Offset: 0, Length: 4, User: &wireUser{ID: 3},
			}),
			mentioned: false,
			text:      "look here",
		},
		{
			name:      "command addressed to the bot",
			msg:       groupMsg("/status@holaclaw", wireEntity{Type: "bot_command", Offset: 0, Length: 16}),
			mentioned: true,
			text:      "/status@holaclaw",
		},
		{
			name:      "command addressed to another bot",
			msg:       groupMsg("/status@otherbot", wireEntity{Type: "bot_command", Offset: 0, Length: 16}),
			mentioned: false,
			text:      "/status@otherbot",
		},
		{
			name:      "no mention at all",
			msg:       groupMsg("just chatting"),
			mentioned: false,
			text:      "just chatting",
		},
		{
			name:      "emoji before the mention keeps utf-16 offsets right",
			msg:       groupMsg("😀 @holaclaw hi", wireEntity{Type: "mention", Offset: 3, Length: 9}),
			mentioned: true,
			text:      "😀 @holaclaw hi",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, ok := a.envelope(t.Context(), []*wireMessage{c.msg})
			if !ok {
				t.Fatal("dropped")
			}
			if env.Mentioned != c.mentioned {
				t.Fatalf("mentioned = %v, want %v", env.Mentioned, c.mentioned)
			}
			if env.Message.Text != c.text {
				t.Fatalf("text = %q, want %q", env.Message.Text, c.text)
			}
		})
	}

	t.Run("reply to the bot counts as a mention", func(t *testing.T) {
		m := groupMsg("and then?")
		m.ReplyToMessage = &wireMessage{MessageID: 1, From: &wireUser{ID: 99, Username: "holaclaw"}}
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if !env.Mentioned {
			t.Fatal("a reply to the bot addresses the bot")
		}
	})

	t.Run("reply to someone else does not", func(t *testing.T) {
		m := groupMsg("and then?")
		m.ReplyToMessage = &wireMessage{MessageID: 1, From: &wireUser{ID: 3}}
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if env.Mentioned {
			t.Fatal("unrelated reply must not be flagged")
		}
	})

	t.Run("caption mention", func(t *testing.T) {
		m := groupMsg("")
		m.Caption = "@holaclaw look"
		m.CaptionEntities = []wireEntity{{Type: "mention", Offset: 0, Length: 9}}
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if !env.Mentioned || env.Message.Text != "look" {
			t.Fatalf("mentioned=%v text=%q", env.Mentioned, env.Message.Text)
		}
	})
}

// TestAttachmentKinds covers every media field the adapter accepts.
func TestAttachmentKinds(t *testing.T) {
	cases := []struct {
		name     string
		msg      func(*wireMessage)
		wantMIME string
		wantKind event.Kind
		wantName string
	}{
		{"photo", func(m *wireMessage) {
			m.Photo = []wirePhoto{{FileID: "small", Width: 90, FileSize: 100}, {FileID: "big", Width: 1280, FileSize: 900}}
		}, "image/jpeg", event.KindImage, "photo.jpg"},
		{"document", func(m *wireMessage) {
			m.Document = &wireFile{FileID: "big", FileName: "report.md", MIMEType: "text/markdown", FileSize: 900}
		}, "text/markdown", event.KindDocument, "report.md"},
		{"audio", func(m *wireMessage) {
			m.Audio = &wireFile{FileID: "big", FileName: "song.mp3", MIMEType: "audio/mpeg", FileSize: 900}
		}, "audio/mpeg", event.KindAudio, "song.mp3"},
		{"voice", func(m *wireMessage) {
			m.Voice = &wireFile{FileID: "big", MIMEType: "audio/ogg", FileSize: 900}
		}, "audio/ogg; codecs=opus", event.KindAudio, "voice.ogg"},
		{"video", func(m *wireMessage) {
			m.Video = &wireFile{FileID: "big", MIMEType: "video/mp4", FileSize: 900}
		}, "video/mp4", event.KindVideo, "video.mp4"},
		{"video note", func(m *wireMessage) {
			m.VideoNote = &wireFile{FileID: "big", FileSize: 900}
		}, "video/mp4", event.KindVideo, "video_note.mp4"},
		{"animation", func(m *wireMessage) {
			m.Animation = &wireFile{FileID: "big", MIMEType: "video/mp4", FileSize: 900}
		}, "video/mp4", event.KindVideo, "animation.mp4"},
		{"unsafe name is sanitised", func(m *wireMessage) {
			m.Document = &wireFile{FileID: "big", FileName: "../../etc/passwd", MIMEType: "text/plain", FileSize: 900}
		}, "text/plain", event.KindDocument, "passwd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			f.on("getFile", wireFile{FileID: "big", FilePath: "docs/file.bin", FileSize: 900})
			f.file("docs/file.bin", []byte(strings.Repeat("x", 900)))
			a, _, _ := newAdapter(t, f, nil)
			a.setBot(99, "holaclaw")

			m := dmMessage(3, "")
			c.msg(m)
			env, ok := a.envelope(t.Context(), []*wireMessage{m})
			if !ok {
				t.Fatal("dropped")
			}
			if len(env.Message.Attachments) != 1 {
				t.Fatalf("attachments = %+v (text %q)", env.Message.Attachments, env.Message.Text)
			}
			att := env.Message.Attachments[0]
			if att.MIME != c.wantMIME || att.Kind != c.wantKind || att.Name != c.wantName {
				t.Fatalf("attachment = %+v", att)
			}
			if att.Size != 900 || att.BlobID == "" || att.SHA256 != att.BlobID || att.ID == "" {
				t.Fatalf("attachment = %+v", att)
			}
		})
	}
}

func TestStickerProducesNoAttachment(t *testing.T) {
	f := newFake(t)
	a, _, _ := newAdapter(t, f, nil)
	a.setBot(99, "holaclaw")
	m := dmMessage(3, "")
	m.Sticker = &wireFile{FileID: "s", MIMEType: "image/webp", FileSize: 10}
	env, ok := a.envelope(t.Context(), []*wireMessage{m})
	if !ok {
		t.Fatal("dropped")
	}
	if len(env.Message.Attachments) != 0 || env.Message.Text != "" {
		t.Fatalf("sticker must be skipped silently: %+v", env.Message)
	}
	if f.count("getFile") != 0 {
		t.Fatal("no download should be attempted for a sticker")
	}
}

func TestAttachmentDownloadCap(t *testing.T) {
	t.Run("declared size over cap", func(t *testing.T) {
		f := newFake(t)
		a, _, _ := newAdapter(t, f, func(o *Options) { o.MaxDownload = 1000 })
		a.setBot(99, "holaclaw")
		m := dmMessage(3, "here")
		m.Document = &wireFile{FileID: "big", FileName: "huge.zip", MIMEType: "application/zip", FileSize: 5000}
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if len(env.Message.Attachments) != 0 {
			t.Fatal("oversize file must not become an attachment")
		}
		if env.Message.Text != "here\n[file too large: huge.zip]" {
			t.Fatalf("text = %q", env.Message.Text)
		}
		if f.count("getFile") != 0 {
			t.Fatal("no getFile for a file we already know is too big")
		}
	})

	t.Run("body longer than the cap", func(t *testing.T) {
		f := newFake(t)
		f.on("getFile", wireFile{FileID: "big", FilePath: "docs/huge.bin"})
		f.file("docs/huge.bin", []byte(strings.Repeat("x", 5000)))
		a, _, _ := newAdapter(t, f, func(o *Options) { o.MaxDownload = 1000 })
		a.setBot(99, "holaclaw")
		m := dmMessage(3, "")
		m.Document = &wireFile{FileID: "big", FileName: "huge.bin", MIMEType: "application/octet-stream"}
		env, _ := a.envelope(t.Context(), []*wireMessage{m})
		if len(env.Message.Attachments) != 0 {
			t.Fatal("oversize body must not become an attachment")
		}
		if env.Message.Text != "[file too large: huge.bin]" {
			t.Fatalf("text = %q", env.Message.Text)
		}
	})
}

func TestAttachmentMIMESniffedWhenMissing(t *testing.T) {
	f := newFake(t)
	f.on("getFile", wireFile{FileID: "big", FilePath: "docs/x"})
	f.file("docs/x", []byte("%PDF-1.4\n%âãÏÓ\n"))
	a, _, _ := newAdapter(t, f, nil)
	a.setBot(99, "holaclaw")
	m := dmMessage(3, "")
	m.Document = &wireFile{FileID: "big", FileName: "x.pdf"}
	env, _ := a.envelope(t.Context(), []*wireMessage{m})
	if len(env.Message.Attachments) != 1 {
		t.Fatalf("attachments = %+v", env.Message.Attachments)
	}
	if !strings.HasPrefix(env.Message.Attachments[0].MIME, "application/pdf") {
		t.Fatalf("mime = %q", env.Message.Attachments[0].MIME)
	}
}

func TestSanitiseName(t *testing.T) {
	cases := map[string]string{
		"report.md":                       "report.md",
		"../../etc/passwd":                "passwd",
		`C:\windows\evil.exe`:             "evil.exe",
		"a\nb.txt":                        "ab.txt",
		"...":                             "",
		strings.Repeat("a", 300) + ".txt": strings.Repeat("a", 120),
	}
	for in, want := range cases {
		if got := sanitiseName(in); got != want {
			t.Errorf("sanitiseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOnJoined(t *testing.T) {
	f := newFake(t)
	var gotChat string
	var gotBy []string
	a, _, _ := newAdapter(t, f, func(o *Options) {
		o.OnJoined = func(_ context.Context, chatID string, by []string) { gotChat, gotBy = chatID, by }
	})
	a.setBot(99, "holaclaw")

	u := &wireChatMemberUpdated{Chat: wireChat{ID: -100777, Type: "supergroup"}, From: &wireUser{ID: 7, Username: "angel"}}
	u.OldChatMember.Status = "left"
	u.NewChatMember.Status = "member"
	u.NewChatMember.User = &wireUser{ID: 99}
	a.handleChatMember(t.Context(), u)
	if gotChat != "-100777" || strings.Join(gotBy, ",") != "tg:7" {
		t.Fatalf("chat = %q by = %v", gotChat, gotBy)
	}

	gotChat = ""
	u.NewChatMember.User = &wireUser{ID: 7} // somebody else joined
	a.handleChatMember(t.Context(), u)
	if gotChat != "" {
		t.Fatal("only the bot's own membership matters")
	}
}
