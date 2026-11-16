package wire

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestErrorMessageShapes(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"nope","type":"invalid_request_error"}}`: "nope",
		`{"error":"flat string"}`:                                     "flat string",
		`{"message":"top level"}`:                                     "top level",
		`plain text body`:                                             "plain text body",
	}
	for body, want := range cases {
		if got := ErrorMessage([]byte(body)); got != want {
			t.Errorf("ErrorMessage(%s) = %q want %q", body, got, want)
		}
	}
	long := strings.Repeat("a", 900)
	if got := ErrorMessage([]byte(long)); len(got) > 420 {
		t.Errorf("long body not truncated: %d bytes", len(got))
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := ParseRetryAfter("30"); got != 30*time.Second {
		t.Errorf("seconds = %v", got)
	}
	when := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := ParseRetryAfter(when); got < 60*time.Second || got > 90*time.Second {
		t.Errorf("http date = %v", got)
	}
	if got := ParseRetryAfter("not a date"); got != 0 {
		t.Errorf("junk = %v", got)
	}
	if got := ParseRetryAfter(""); got != 0 {
		t.Errorf("empty = %v", got)
	}
}

func TestMentionsContext(t *testing.T) {
	cases := map[string]bool{
		"maximum context length exceeded": true,
		"the prompt is too long":          true,
		"something else entirely":         false,
	}
	for msg, want := range cases {
		if got := MentionsContext(msg); got != want {
			t.Errorf("MentionsContext(%q) = %v want %v", msg, got, want)
		}
	}
}
