package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrDenied is returned by Call, before anything is written, for a method the
// stealth rule forbids (docs/browser.md).
var ErrDenied = errors.New("cdp: method denied by the stealth rule")

// isolatedWorld is the name of the world EvalIsolated runs in. Deliberately
// bland: a world name is visible to the page's own frame bookkeeping in some
// paths, and "holaclaw" there would defeat the point.
const isolatedWorld = "util"

// denied lists methods that are never sent, whatever the parameters.
//
//   - Runtime.enable turns on the Runtime domain, which a page can observe
//     (the "runtimeEnableLeak" check): it is the single best-known automation
//     tell, and nothing here needs it — Runtime.evaluate works without it.
//   - Console.enable is Runtime.enable by another name: Chromium's Console
//     domain enables Runtime underneath.
//
// Log.enable is deliberately NOT here: the Log domain does not instrument a
// page's main world. We simply never call it.
var denied = map[string]string{
	"Runtime.enable": "the Runtime domain is observable by the page",
	"Console.enable": "enables the Runtime domain underneath",
}

// mainWorld lists methods that touch a world, and the parameter that has to
// name a non-main one. Without it the method would run script in the page's
// own world, where a page can see it (the "mainWorldExecution" check).
var mainWorld = []struct {
	method string
	fields []string
	why    string
}{
	{"Runtime.evaluate", []string{"contextId", "uniqueContextId"}, "needs an isolated contextId — use EvalIsolated"},
	{"Runtime.callFunctionOn", []string{"executionContextId", "uniqueContextId", "objectId"}, "needs an isolated context or an object resolved in one"},
	{"Runtime.compileScript", []string{"executionContextId"}, "needs an isolated executionContextId"},
	{"Runtime.runScript", []string{"executionContextId"}, "needs an isolated executionContextId"},
	{"Runtime.addBinding", []string{"executionContextName"}, "a binding without a world name lands in the main world"},
	{"Page.addScriptToEvaluateOnNewDocument", []string{"worldName"}, "a script without a worldName runs in the main world"},
}

// checkDenied is the rule, in code. It runs on every Call before anything is
// written, so there is no sanctioned way around it — EvalIsolated passes
// because it genuinely carries an isolated contextId.
func checkDenied(method string, params json.RawMessage) error {
	if why, bad := denied[method]; bad {
		return fmt.Errorf("%w: %s: %s", ErrDenied, method, why)
	}
	for _, m := range mainWorld {
		if m.method != method {
			continue
		}
		fields, err := paramFields(params)
		if err != nil {
			return fmt.Errorf("%w: %s: parameters are not an object", ErrDenied, method)
		}
		for _, f := range m.fields {
			if present(fields[f]) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s: %s", ErrDenied, method, m.why)
	}
	return nil
}

func paramFields(params json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(params)) == 0 {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// present reports whether a parameter was given a usable value: null, "", 0
// and absent all mean "the main world".
func present(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	switch s {
	case "", "null", `""`, "0":
		return false
	}
	return true
}

// EvalIsolated is the one sanctioned way to run JavaScript: a fresh isolated
// world for the frame, then Runtime.evaluate against that world's context.
// Runtime is never enabled — evaluate does not need it — and the page's main
// world sees neither the expression nor anything it defines.
//
// result may be nil; otherwise it receives the returned value (returnByValue),
// so the expression must evaluate to something JSON can carry.
//
// The world is created on every call rather than cached: Page.createIsolatedWorld
// is cheap and always answers with a context that is current for the frame,
// which a cache would not be across a navigation.
func (c *Conn) EvalIsolated(ctx context.Context, sessionID, frameID, expression string, result any) error {
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	// "grantUniveralAccess" is the protocol's own spelling, typo included.
	err := c.Call(ctx, sessionID, "Page.createIsolatedWorld", map[string]any{
		"frameId":             frameID,
		"worldName":           isolatedWorld,
		"grantUniveralAccess": false,
	}, &world)
	if err != nil {
		return err
	}
	if world.ExecutionContextID == 0 {
		return errors.New("cdp: createIsolatedWorld returned no context")
	}

	var reply struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err = c.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"contextId":     world.ExecutionContextID,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &reply)
	if err != nil {
		return err
	}
	if d := reply.ExceptionDetails; d != nil {
		if d.Exception != nil && d.Exception.Description != "" {
			return fmt.Errorf("cdp: evaluate: %s", d.Exception.Description)
		}
		return fmt.Errorf("cdp: evaluate: %s", d.Text)
	}
	if result == nil || len(reply.Result.Value) == 0 {
		return nil
	}
	if err := json.Unmarshal(reply.Result.Value, result); err != nil {
		return fmt.Errorf("cdp: evaluate result: %w", err)
	}
	return nil
}

// EvalIsolated runs expression in an isolated world of frameID. See
// Conn.EvalIsolated.
func (s *Session) EvalIsolated(ctx context.Context, frameID, expression string, result any) error {
	return s.c.EvalIsolated(ctx, s.id, frameID, expression, result)
}
