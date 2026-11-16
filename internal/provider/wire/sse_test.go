package wire

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReaderFraming(t *testing.T) {
	body := ": keep-alive\r\n" +
		"event: message\r\n" +
		"data: {\"a\":1}\r\n" +
		"\r\n" +
		"\r\n" + // stray blank line
		"data: line one\n" +
		"data: line two\n" +
		"\n" +
		"id: 7\n" +
		"data: [DONE]\n\n"
	r := NewSSEReader(strings.NewReader(body))
	want := []string{`{"a":1}`, "line one\nline two", "[DONE]"}
	for i, w := range want {
		_, got, err := r.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if got != w {
			t.Fatalf("event %d = %q want %q", i, got, w)
		}
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("err = %v want EOF", err)
	}
}

func TestSSEReaderEventName(t *testing.T) {
	body := "event: response.completed\n" +
		"data: {\"a\":1}\n\n" +
		"data: no-name\n\n"
	r := NewSSEReader(strings.NewReader(body))
	name, data, err := r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if name != "response.completed" || data != `{"a":1}` {
		t.Fatalf("event = %q data = %q", name, data)
	}
	name, data, err = r.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if name != "" || data != "no-name" {
		t.Fatalf("event = %q data = %q", name, data)
	}
}

func TestSSEReaderLargeLineAndUnterminatedTail(t *testing.T) {
	big := strings.Repeat("x", 512<<10)
	r := NewSSEReader(strings.NewReader("data: " + big + "\n\ndata: tail"))
	_, got, err := r.Next()
	if err != nil || got != big {
		t.Fatalf("large line: len=%d err=%v", len(got), err)
	}
	_, got, err = r.Next()
	if err != nil || got != "tail" {
		t.Fatalf("tail event = %q err=%v", got, err)
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Fatalf("err = %v want EOF", err)
	}
}
