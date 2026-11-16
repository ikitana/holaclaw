package provider

import "context"

// QuestionType is the kind of typed answer a decision model gives
// (docs/models.md, "Decision models").
type QuestionType string

const (
	// QuestionNoul asks yes or no; the answer is the probability of yes.
	QuestionNoul QuestionType = "noul"
	// QuestionChoice asks for one of the keys of Criteria.
	QuestionChoice QuestionType = "choice"
	// QuestionScore asks for a position on the ordered Scale.
	QuestionScore QuestionType = "score"
)

// Question is one narrow, typed question about a state. The caller owns the
// workflow; the model only answers.
type Question struct {
	Type         QuestionType
	Instructions string
	// Criteria describes each possible answer: the keys "true" and "false" for
	// a noul (optional), the choices themselves for a choice (required).
	Criteria map[string]string
	// Scale is the ordered legend of a score, lowest first.
	Scale []string
}

// Answer is the reply to one Question. Only the fields of its Type are set.
// Every string in it comes from the closed set the caller sent — a transport
// rejects anything else — so an Answer is safe to log.
type Answer struct {
	Type QuestionType
	// Noul is the probability of yes, 0..1.
	Noul float64
	// Choice is one key of the question's Criteria.
	Choice string
	// Score is the position on the scale, 0..len(Scale)-1.
	Score float64
	// Probabilities is the distribution over choices, or over scale indexes
	// ("0", "1", …). May be nil.
	Probabilities map[string]float64
	// Confidence is 0..1; zero when the upstream does not report one.
	Confidence float64
}

// DecisionRequest is one call: a state and the questions about it, by name.
type DecisionRequest struct {
	Model     Model
	State     string
	Questions map[string]Question
}

// DecisionResponse carries exactly one Answer per question asked.
type DecisionResponse struct {
	Answers map[string]Answer
	Usage   Usage
}

// Decider is implemented by transports that serve decision models. It is an
// experimental contract (the only upstream is an alpha API): a Decider never
// retries, and every caller must define what happens when it fails. Errors are
// *Error values, like a Provider's. A reply that arrived but failed validation
// comes back as an error together with a response holding only its Usage, so
// the caller can still count a call that was made and paid for.
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error)
}
