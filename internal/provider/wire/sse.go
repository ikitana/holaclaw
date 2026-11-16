// Package wire holds the on-the-wire helpers shared by the OpenAI-compatible
// chat-completions provider and the Responses-API provider: SSE framing,
// reasoning-tag scrubbing, tool-argument repair and error-message extraction.
package wire

import (
	"bufio"
	"io"
	"strings"
)

// SSEReader reads text/event-stream framing. It uses bufio.Reader rather than
// Scanner because a single data line can hold a whole tool-call payload and
// must not hit a token-size limit.
//
// It is event-name aware: Next returns the `event:` name alongside the data
// payload, because the Responses API discriminates frames by that name.
// Chat-completions streams never set it and callers there simply ignore it.
type SSEReader struct {
	br   *bufio.Reader
	buf  strings.Builder // accumulated data: lines of the current event
	name string          // event: name of the current event
}

// NewSSEReader wraps r in an SSEReader.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{br: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the event name and data payload of the next event. It returns
// io.EOF when the stream ends; a trailing event without its blank line is
// still delivered.
func (s *SSEReader) Next() (event, data string, err error) {
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF && s.buf.Len() > 0 {
				return s.take()
			}
			return "", "", err
		}
		switch {
		case line == "": // event boundary
			if s.buf.Len() == 0 && s.name == "" {
				continue // keep-alive blank line
			}
			return s.take()
		case strings.HasPrefix(line, ":"): // comment / keep-alive
			continue
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			if s.buf.Len() > 0 {
				s.buf.WriteByte('\n')
			}
			s.buf.WriteString(v)
		case strings.HasPrefix(line, "event:"):
			v := strings.TrimPrefix(line, "event:")
			s.name = strings.TrimSpace(v)
		default:
			continue // id:, retry: and unknown fields are ignored
		}
	}
}

func (s *SSEReader) take() (string, string, error) {
	name, data := s.name, s.buf.String()
	s.name = ""
	s.buf.Reset()
	return name, data, nil
}

// readLine returns one line without its CR/LF terminator.
func (s *SSEReader) readLine() (string, error) {
	line, err := s.br.ReadString('\n')
	if err != nil {
		if err == io.EOF && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
