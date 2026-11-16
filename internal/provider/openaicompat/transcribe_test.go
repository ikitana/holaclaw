package openaicompat

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"

	"holaclaw/internal/provider"
)

func TestTranscribeMultipart(t *testing.T) {
	var gotModel, gotName, gotFile, gotCT string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		gotModel = r.FormValue("model")
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer func() { _ = f.Close() }()
		gotName = hdr.Filename
		b, _ := io.ReadAll(f)
		gotFile = string(b)
		_, _ = io.WriteString(w, `{"text":"hola qué tal"}`)
	}), Options{})

	txt, usage, err := c.Transcribe(context.Background(), provider.Model{ID: "whisper-1"}, "audio/ogg", []byte("OggS-audio"))
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if !reflect.DeepEqual(usage, provider.Usage{}) {
		t.Errorf("no usage object = %s, want zero", usageText(usage))
	}
	if txt != "hola qué tal" {
		t.Errorf("text = %q", txt)
	}
	if gotModel != "whisper-1" || gotFile != "OggS-audio" || gotName != "audio.ogg" {
		t.Errorf("model=%q file=%q name=%q", gotModel, gotFile, gotName)
	}
	if gotCT == "" || gotCT[:19] != "multipart/form-data" {
		t.Errorf("content-type = %q", gotCT)
	}
}

func TestAudioFilename(t *testing.T) {
	cases := map[string]string{
		"audio/mpeg":             "audio.mp3",
		"audio/ogg; codecs=opus": "audio.ogg",
		"audio/wav":              "audio.wav",
		"audio/webm":             "audio.webm",
		"audio/mp4":              "audio.m4a",
		"audio/flac":             "audio.flac",
		"":                       "audio.mp3",
	}
	for mime, want := range cases {
		if got := audioFilename(mime); got != want {
			t.Errorf("audioFilename(%q) = %q want %q", mime, got, want)
		}
	}
}

func TestTranscribeHTTPError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"model not loaded"}}`)
	}), Options{})
	_, _, err := c.Transcribe(context.Background(), provider.Model{ID: "w"}, "audio/mpeg", []byte("x"))
	if provider.KindOf(err) != provider.ErrServer {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

// A token-billed model reports tokens; a duration-billed one seconds, which
// are not tokens and are dropped; a cost comes through when sent.
func TestTranscribeUsage(t *testing.T) {
	cases := []struct {
		name, usage string
		want        provider.Usage
	}{
		{"tokens", `{"type":"tokens","input_tokens":14,"output_tokens":45,"total_tokens":59,"input_token_details":{"text_tokens":0,"audio_tokens":14}}`,
			provider.Usage{PromptTokens: 14, CompletionTokens: 45}},
		{"tokens, cached and priced", `{"type":"tokens","input_tokens":14,"output_tokens":45,"input_token_details":{"cached_tokens":4},"cost":0.0002}`,
			provider.Usage{PromptTokens: 14, CompletionTokens: 45, CachedTokens: ip(4), Cost: nano(200000)}},
		{"duration", `{"type":"duration","seconds":9}`, provider.Usage{}},
		{"duration, priced", `{"type":"duration","seconds":9,"cost":0}`, provider.Usage{Cost: nano(0)}},
		{"exponent", `{"type":"duration","seconds":9,"cost":2E-4}`, provider.Usage{Cost: nano(200000)}},
		{"malformed", `{"type":"duration","seconds":9,"cost":{}}`, provider.Usage{}},
	}
	for _, tc := range cases {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"text":"hola","usage":`+tc.usage+`}`)
		}), Options{})
		txt, usage, err := c.Transcribe(context.Background(), provider.Model{ID: "w"}, "audio/mpeg", []byte("x"))
		if err != nil || txt != "hola" {
			t.Fatalf("%s: %q %v", tc.name, txt, err)
		}
		if !reflect.DeepEqual(usage, tc.want) {
			t.Errorf("%s: usage = %s, want %s", tc.name, usageText(usage), usageText(tc.want))
		}
	}
}
