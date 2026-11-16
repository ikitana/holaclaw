package main

// The little face at the top right of the chat. It is there for one reason:
// the state of a turn is the thing somebody glances up for, and a word alone
// reads the same whether it changed a second ago or a minute ago. A face that
// moves says the chat is alive; the word beside it says what it is doing.

// uiState is what the chat is up to, as the header says it. It is not the
// turn's state machine — the engine owns that — only the handful of things
// worth a word on screen.
type uiState int

const (
	stateIdle     uiState = iota // nothing is running
	stateThinking                // a model is composing
	stateWorking                 // a tool call is in flight
	stateWaiting                 // an approval is on screen
	stateAborting                // the turn is being stopped
)

// word is the one word under the face.
func (s uiState) word() string {
	switch s {
	case stateThinking:
		return "thinking"
	case stateWorking:
		return "working"
	case stateWaiting:
		return "waiting"
	case stateAborting:
		return "aborting"
	}
	return "ready"
}

// mascotFrames is the animation of each state: two frames that differ in one
// or two characters, so the face reads as the same face blinking rather than
// as a different one each tick. Every frame of a state is the same width, so
// the right margin does not jitter.
var mascotFrames = map[uiState][]string{
	stateIdle:     {"(•‿•)", "(•_•)"},
	stateThinking: {"(•_•)?", "(•_•)…"},
	stateWorking:  {"(⌐■_■)", "(⌐■‿■)"},
	stateWaiting:  {"(•o•)!", "(°o°)!"},
	stateAborting: {"(x_x)", "(×_×)"},
}

// mascotFrame is the face of one state at one tick. It is pure so the header
// can be drawn from nothing but the model's counter, and so it can be tested
// without a terminal.
func mascotFrame(state uiState, tick int) string {
	frames, ok := mascotFrames[state]
	if !ok || len(frames) == 0 {
		frames = mascotFrames[stateIdle]
	}
	n := len(frames)
	// The counter only ever goes up, but a negative one must still land on a
	// frame rather than off the end of the slice.
	return frames[((tick%n)+n)%n]
}
