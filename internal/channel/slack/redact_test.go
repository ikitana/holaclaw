package slack

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Slack hands out one-shot credentials in URL query strings: the Socket Mode
// ticket and the signed file upload URL. A failure quotes the whole URL, and
// that text becomes a returned error, a channel status detail and a log line —
// so the query never survives redact.

const (
	fakeTicket    = "TICKET-e3f19a7c-do-not-log"
	fakeUploadTok = "UPLOADTOK-91bc4d2e-do-not-log"
)

func TestStripURLSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			"socket ticket",
			`failed to WebSocket dial: Get "wss://wss.slack.com/link/?ticket=` + fakeTicket + `&app_id=A1": EOF`,
			`failed to WebSocket dial: Get "wss://wss.slack.com/link/?[redacted]": EOF`,
		},
		{
			"upload url",
			`Post "https://files.slack.com/upload/v1/ABC?t=` + fakeUploadTok + `": connection reset`,
			`Post "https://files.slack.com/upload/v1/ABC?[redacted]": connection reset`,
		},
		{
			"userinfo",
			`Get https://bot:hunter2@slack.example/api/x: nope`,
			`Get https://[redacted]@slack.example/api/x: nope`,
		},
		{
			"fragment",
			`open wss://host/path#` + fakeTicket,
			`open wss://host/path?[redacted]`,
		},
		{
			"nothing to strip",
			`chat.postMessage: channel_not_found`,
			`chat.postMessage: channel_not_found`,
		},
		{
			"plain url is left whole",
			`see https://api.slack.com/docs for the rules`,
			`see https://api.slack.com/docs for the rules`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripURLSecrets(tc.in); got != tc.want {
				t.Fatalf("stripURLSecrets(%q)\n = %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// A dial failure must not carry the connection ticket into the status detail,
// the reconnect log line or anything else the adapter reports.
func TestSocketTicketNeverLeaves(t *testing.T) {
	f := newFakeSlack(t)
	// Port 1 refuses: the dial fails with the URL quoted in its error.
	ticketURL := "ws://127.0.0.1:1/link/?ticket=" + fakeTicket
	f.set("apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		f.ok(w, `{"url":`+jsonString(ticketURL)+`}`)
	})
	a, logs := newAdapter(t, f, nil)

	st, stop := run(t, a, nil)
	s := waitStatus(t, st, channel.StateReconnecting)
	_ = stop()

	if strings.Contains(s.msg, fakeTicket) {
		t.Fatalf("the ticket reached the status detail: %q", s.msg)
	}
	if !strings.Contains(s.msg, "127.0.0.1:1") {
		t.Fatalf("the status detail lost its usable part: %q", s.msg)
	}
	if strings.Contains(logs.String(), fakeTicket) {
		t.Fatalf("the ticket reached the log:\n%s", logs.String())
	}
	assertNoTokens(t, s.msg, logs.String())
}

// The same for the upload URL: an upload that fails on the wire degrades the
// attachment, and the degraded note must not quote the signed URL.
func TestUploadURLNeverLeaves(t *testing.T) {
	f := newFakeSlack(t)
	uploadURL := "http://127.0.0.1:1/upload?t=" + fakeUploadTok
	f.set("files.getUploadURLExternal", func(w http.ResponseWriter, _ *http.Request) {
		f.ok(w, `{"upload_url":`+jsonString(uploadURL)+`,"file_id":"F1"}`)
	})
	a := mappingAdapter(t, f, func(o *Options) {
		o.Blobs = memBlobs{data: map[string][]byte{"b1": []byte("filebytes")}}
	})

	rec, err := a.Send(context.Background(), channel.Outbound{
		Target: target(),
		Message: event.Message{
			Text:        "here you go",
			Attachments: []event.Attachment{{ID: "A1", BlobID: "b1", Name: "doc.txt"}},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(rec.Degraded) != 1 {
		t.Fatalf("degraded = %v, want one entry", rec.Degraded)
	}
	if strings.Contains(rec.Degraded[0], fakeUploadTok) {
		t.Fatalf("the upload token reached the degraded note: %q", rec.Degraded[0])
	}
	assertNoTokens(t, rec.Degraded[0])
}
