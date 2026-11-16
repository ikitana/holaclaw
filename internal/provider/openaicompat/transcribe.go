package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"

	"holaclaw/internal/provider"
)

// transcribeUsage is the usage object of a transcription. A token-billed
// model (gpt-4o-transcribe) reports type "tokens" with input and output
// tokens; a duration-billed one (whisper) reports type "duration" with
// seconds, which are not tokens and are left out. Cost is OpenRouter's.
type transcribeUsage struct {
	Type              string          `json:"type"`
	InputTokens       int             `json:"input_tokens"`
	OutputTokens      int             `json:"output_tokens"`
	Cost              json.RawMessage `json:"cost"`
	InputTokenDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_token_details"`
}

func (u *transcribeUsage) usage() provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	out := provider.Usage{Cost: provider.WireCost(u.Cost)}
	if u.Type == "tokens" {
		out.PromptTokens, out.CompletionTokens = u.InputTokens, u.OutputTokens
		if u.InputTokenDetails != nil {
			out.CachedTokens = u.InputTokenDetails.CachedTokens
		}
	}
	return out
}

// Transcribe uploads audio to an OpenAI-compatible STT endpoint.
func (c *Client) Transcribe(ctx context.Context, model provider.Model, mimeType string, audio []byte) (string, provider.Usage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", audioFilename(mimeType))
	if err != nil {
		return "", provider.Usage{}, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "building upload", Err: err}
	}
	if _, err := fw.Write(audio); err != nil {
		return "", provider.Usage{}, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "building upload", Err: err}
	}
	if err := mw.WriteField("model", model.ID); err != nil {
		return "", provider.Usage{}, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "building upload", Err: err}
	}
	if err := mw.Close(); err != nil {
		return "", provider.Usage{}, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "building upload", Err: err}
	}
	var out struct {
		Text  string           `json:"text"`
		Usage *transcribeUsage `json:"usage"`
	}
	if err := c.doJSON(ctx, model, http.MethodPost, "/audio/transcriptions", mw.FormDataContentType(), &buf, &out); err != nil {
		return "", provider.Usage{}, err
	}
	return out.Text, out.Usage.usage(), nil
}

// audioFilename gives the upload a name with an extension the endpoint can use
// to identify the codec.
func audioFilename(mimeType string) string {
	if mimeType == "" {
		mimeType = defaultTranscribeMIME
	}
	base, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		base = mimeType
	}
	switch base {
	case "audio/mpeg", "audio/mp3":
		return "audio.mp3"
	case "audio/ogg", "audio/opus":
		return "audio.ogg"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "audio.wav"
	case "audio/webm":
		return "audio.webm"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "audio.m4a"
	case "audio/flac":
		return "audio.flac"
	}
	if exts, err := mime.ExtensionsByType(base); err == nil && len(exts) > 0 {
		return "audio" + exts[0]
	}
	return defaultTranscribeFname
}
