package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"

	"holaclaw/internal/provider"
)

// wireRequest is the decisions request body.
type wireRequest struct {
	Model     string                  `json:"model"`
	State     string                  `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

// wireQuestion carries criteria as raw JSON: an object for noul and choice, an
// ordered array for score.
type wireQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// wireResponse is the 200 reply. Fields we do not map (id, provider, the score
// legend) are ignored on purpose.
type wireResponse struct {
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int             `json:"input_tokens"`
		OutputTokens int             `json:"output_tokens"`
		Cost         json.RawMessage `json:"cost"` // provider.WireCost
	} `json:"usage"`
}

// wireAnswer keeps the numbers as pointers so a missing field is told apart
// from a zero one.
type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

// Decide asks the model every question about the state and returns one answer
// each. The request is validated before it is sent and the reply is validated
// against the questions asked, so every string in an Answer comes from the
// closed set the caller sent. There are no retries. Answers that fail
// validation return the error with a response carrying only the usage.
func (c *Client) Decide(ctx context.Context, req provider.DecisionRequest) (*provider.DecisionResponse, error) {
	body, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	raw, err := c.post(ctx, req.Model, body)
	if err != nil {
		return nil, err
	}
	var out wireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.invalidResponse("malformed JSON")
	}
	usage := provider.Usage{
		PromptTokens:     out.Usage.InputTokens,
		CompletionTokens: out.Usage.OutputTokens,
		Cost:             provider.WireCost(out.Usage.Cost),
	}
	answers, err := c.validateAnswers(req.Questions, out.Answers)
	if err != nil {
		return &provider.DecisionResponse{Usage: usage}, err
	}
	return &provider.DecisionResponse{Answers: answers, Usage: usage}, nil
}

// buildRequest validates req and encodes the body. A malformed request never
// reaches the network.
func (c *Client) buildRequest(req provider.DecisionRequest) ([]byte, error) {
	if req.Model.URL == "" {
		return nil, c.badRequest("model has no url")
	}
	if len(req.Questions) == 0 {
		return nil, c.badRequest("no questions")
	}
	qs := make(map[string]wireQuestion, len(req.Questions))
	for name, q := range req.Questions {
		if name == "" {
			return nil, c.badRequest("question with an empty name")
		}
		if q.Instructions == "" {
			return nil, c.badRequest("question " + name + ": no instructions")
		}
		wq := wireQuestion{Type: string(q.Type), Instructions: q.Instructions}
		switch q.Type {
		case provider.QuestionNoul:
			if len(q.Criteria) > 0 {
				wq.Criteria = mustJSON(q.Criteria)
			}
		case provider.QuestionChoice:
			if len(q.Criteria) < 2 {
				return nil, c.badRequest("question " + name + ": a choice needs at least two criteria")
			}
			wq.Criteria = mustJSON(q.Criteria)
		case provider.QuestionScore:
			if len(q.Scale) < 2 {
				return nil, c.badRequest("question " + name + ": a score needs at least two scale entries")
			}
			wq.Criteria = mustJSON(q.Scale)
		default:
			return nil, c.badRequest("question " + name + ": unknown type")
		}
		qs[name] = wq
	}
	body, err := json.Marshal(wireRequest{Model: req.Model.ID, State: req.State, Questions: qs})
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "encoding request", Err: err}
	}
	return body, nil
}

// post sends one request under the concurrency limit and returns the body.
func (c *Client) post(ctx context.Context, model provider.Model, body []byte) ([]byte, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout(model))
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, model.URL, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "bad request url", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != nil {
		if key := c.apiKey(); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, rctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, c.transportError(ctx, rctx, err)
	}
	if len(raw) > MaxResponseBytes {
		return nil, c.invalidResponse("response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.httpError(resp, raw)
	}
	return raw, nil
}

// validateAnswers checks the reply against the questions asked: exactly one
// answer per question, of the right type, with every string and every number
// inside the closed set the caller sent. Extra answers are ignored.
func (c *Client) validateAnswers(qs map[string]provider.Question, got map[string]wireAnswer) (map[string]provider.Answer, error) {
	answers := make(map[string]provider.Answer, len(qs))
	for name, q := range qs {
		wa, ok := got[name]
		if !ok {
			return nil, c.invalidResponse("missing answer")
		}
		if wa.Type != string(q.Type) {
			return nil, c.invalidResponse("answer type does not match the question")
		}
		a := provider.Answer{Type: q.Type}
		if wa.Confidence != nil {
			if !inUnit(*wa.Confidence) {
				return nil, c.invalidResponse("confidence out of range")
			}
			a.Confidence = *wa.Confidence
		}
		switch q.Type {
		case provider.QuestionNoul:
			if wa.Noul == nil || !inUnit(*wa.Noul) {
				return nil, c.invalidResponse("noul out of range")
			}
			a.Noul = *wa.Noul
		case provider.QuestionChoice:
			if _, ok := q.Criteria[wa.Choice]; !ok {
				return nil, c.invalidResponse("choice is not one of the criteria")
			}
			a.Choice = wa.Choice
			probs, err := c.validateProbs(wa.Probabilities, func(k string) bool {
				_, ok := q.Criteria[k]
				return ok
			})
			if err != nil {
				return nil, err
			}
			a.Probabilities = probs
		case provider.QuestionScore:
			if wa.Score == nil || !inRange(*wa.Score, 0, float64(len(q.Scale)-1)) {
				return nil, c.invalidResponse("score out of range")
			}
			a.Score = *wa.Score
			probs, err := c.validateProbs(wa.Probabilities, func(k string) bool {
				i, err := strconv.Atoi(k)
				return err == nil && i >= 0 && i < len(q.Scale)
			})
			if err != nil {
				return nil, err
			}
			a.Probabilities = probs
		default:
			return nil, c.invalidResponse("unknown answer type")
		}
		answers[name] = a
	}
	return answers, nil
}

// validateProbs keeps a distribution only when every key is one the caller sent
// and every value is a real number in [0,1].
func (c *Client) validateProbs(probs map[string]float64, known func(string) bool) (map[string]float64, error) {
	if len(probs) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(probs))
	for k, v := range probs {
		if !known(k) {
			return nil, c.invalidResponse("probability key is not one of the criteria")
		}
		if !inUnit(v) {
			return nil, c.invalidResponse("probability out of range")
		}
		out[k] = v
	}
	return out, nil
}

// inUnit reports whether v is a real number in [0,1].
func inUnit(v float64) bool { return inRange(v, 0, 1) }

// inRange reports whether v is a real number in [lo,hi].
func inRange(v, lo, hi float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	return v >= lo && v <= hi
}

// mustJSON encodes a value that cannot fail to encode (maps of strings and
// string slices).
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
