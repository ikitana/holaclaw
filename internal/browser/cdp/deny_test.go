package cdp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestDenylist is the rule of this package under test: whatever a caller does,
// Runtime is never enabled and nothing reaches the main world.
func TestDenylist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		params any
		denied bool
	}{
		{"runtime enable", "Runtime.enable", nil, true},
		{"runtime enable with params", "Runtime.enable", map[string]any{"x": 1}, true},
		{"console enable", "Console.enable", nil, true},

		{"evaluate in the main world", "Runtime.evaluate", map[string]any{"expression": "1"}, true},
		{"evaluate with a null contextId", "Runtime.evaluate", map[string]any{"expression": "1", "contextId": nil}, true},
		{"evaluate with a zero contextId", "Runtime.evaluate", map[string]any{"expression": "1", "contextId": 0}, true},
		{"evaluate with an empty uniqueContextId", "Runtime.evaluate", map[string]any{"expression": "1", "uniqueContextId": ""}, true},
		{"evaluate in an isolated world", "Runtime.evaluate", map[string]any{"expression": "1", "contextId": 4}, false},
		{"evaluate by unique context", "Runtime.evaluate", map[string]any{"expression": "1", "uniqueContextId": "7.2"}, false},

		{"callFunctionOn loose", "Runtime.callFunctionOn", map[string]any{"functionDeclaration": "function(){}"}, true},
		{"callFunctionOn on an object", "Runtime.callFunctionOn", map[string]any{"functionDeclaration": "function(){}", "objectId": "1.2.3"}, false},
		{"callFunctionOn in a context", "Runtime.callFunctionOn", map[string]any{"functionDeclaration": "function(){}", "executionContextId": 9}, false},

		{"compileScript loose", "Runtime.compileScript", map[string]any{"expression": "1"}, true},
		{"compileScript in a context", "Runtime.compileScript", map[string]any{"expression": "1", "executionContextId": 3}, false},
		{"runScript loose", "Runtime.runScript", map[string]any{"scriptId": "1"}, true},
		{"runScript in a context", "Runtime.runScript", map[string]any{"scriptId": "1", "executionContextId": 3}, false},

		{"addBinding without a world", "Runtime.addBinding", map[string]any{"name": "b"}, true},
		{"addBinding in a named world", "Runtime.addBinding", map[string]any{"name": "b", "executionContextName": "util"}, false},

		{"new-document script without a world", "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "1"}, true},
		{"new-document script with an empty world", "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "1", "worldName": ""}, true},
		{"new-document script in a world", "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "1", "worldName": "util"}, false},

		// Everything else is untouched.
		{"page enable", "Page.enable", nil, false},
		{"log enable", "Log.enable", nil, false},
		{"createIsolatedWorld", "Page.createIsolatedWorld", map[string]any{"frameId": "F", "worldName": "util"}, false},
		{"getFullAXTree", "Accessibility.getFullAXTree", nil, false},
		{"dispatchMouseEvent", "Input.dispatchMouseEvent", MouseEvent{Type: "mousePressed"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
				f.reply(cmd.ID, map[string]any{})
			})
			err := f.conn.Call(ctxT(t), "S1", tc.method, tc.params, nil)
			switch {
			case tc.denied && !errors.Is(err, ErrDenied):
				t.Fatalf("%s: err = %v, want ErrDenied", tc.method, err)
			case tc.denied:
				if !strings.Contains(err.Error(), tc.method) {
					t.Fatalf("error does not name the method: %v", err)
				}
				if n := len(f.commands()); n != 0 {
					t.Fatalf("a denied method was written to the pipe: %+v", f.commands())
				}
			case !tc.denied && err != nil:
				t.Fatalf("%s: err = %v, want nil", tc.method, err)
			}
		})
	}
}

// TestDenylistRejectsNonObjectParams makes sure the check cannot be slipped
// past by sending parameters it cannot inspect.
func TestDenylistRejectsNonObjectParams(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil)
	err := f.conn.Call(ctxT(t), "S1", "Runtime.evaluate", json.RawMessage(`["contextId", 4]`), nil)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if n := len(f.commands()); n != 0 {
		t.Fatalf("something was written: %+v", f.commands())
	}
}

// TestNoEnableIsEverSent walks the typed helpers that touch a page and checks
// nothing in the package reaches for Runtime or Console.
func TestHelpersNeverEnableRuntime(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		switch cmd.Method {
		case "Page.createIsolatedWorld":
			f.reply(cmd.ID, map[string]any{"executionContextId": 42})
		case "Runtime.evaluate":
			f.reply(cmd.ID, map[string]any{"result": map[string]any{"value": "ok"}})
		default:
			f.reply(cmd.ID, map[string]any{})
		}
	})

	ctx := ctxT(t)
	s := f.conn.Session("S1")
	var errs []error
	errs = append(errs,
		s.PageEnable(ctx),
		s.SetLifecycleEventsEnabled(ctx, true),
		s.SetAutoAttach(ctx, true, false),
		s.AccessibilityEnable(ctx),
		s.FetchEnable(ctx, []RequestPattern{{URLPattern: "*"}}),
		s.EvalIsolated(ctx, "F1", "1+1", nil),
	)
	for _, err := range errs {
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
	}
	for _, c := range f.commands() {
		if strings.HasPrefix(c.Method, "Runtime.") && c.Method != "Runtime.evaluate" {
			t.Fatalf("unexpected Runtime call: %s", c.Method)
		}
		if strings.HasPrefix(c.Method, "Console.") || c.Method == "Runtime.enable" {
			t.Fatalf("forbidden call: %s", c.Method)
		}
	}
}

func TestEvalIsolatedSequence(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		switch cmd.Method {
		case "Page.createIsolatedWorld":
			var p struct {
				FrameID   string `json:"frameId"`
				WorldName string `json:"worldName"`
			}
			if err := json.Unmarshal(cmd.Params, &p); err != nil {
				t.Errorf("createIsolatedWorld params: %v", err)
			}
			if p.FrameID != "F1" {
				t.Errorf("frameId = %q", p.FrameID)
			}
			if p.WorldName == "" || strings.Contains(strings.ToLower(p.WorldName), "holaclaw") {
				t.Errorf("world name %q must be set and must not name us", p.WorldName)
			}
			f.reply(cmd.ID, map[string]any{"executionContextId": 42})
		case "Runtime.evaluate":
			var p struct {
				Expression    string `json:"expression"`
				ContextID     int64  `json:"contextId"`
				ReturnByValue bool   `json:"returnByValue"`
			}
			if err := json.Unmarshal(cmd.Params, &p); err != nil {
				t.Errorf("evaluate params: %v", err)
			}
			if p.ContextID != 42 || !p.ReturnByValue {
				t.Errorf("evaluate params = %+v", p)
			}
			f.reply(cmd.ID, map[string]any{"result": map[string]any{"type": "number", "value": 3}})
		default:
			t.Errorf("unexpected method %s", cmd.Method)
			f.reply(cmd.ID, map[string]any{})
		}
	})

	var got int
	if err := f.conn.EvalIsolated(ctxT(t), "S1", "F1", "1+2", &got); err != nil {
		t.Fatalf("EvalIsolated: %v", err)
	}
	if got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
	methods := []string{}
	for _, c := range f.commands() {
		methods = append(methods, c.Method)
	}
	want := "Page.createIsolatedWorld,Runtime.evaluate"
	if strings.Join(methods, ",") != want {
		t.Fatalf("sequence = %v, want %s", methods, want)
	}
	// And the result may be discarded.
	if err := f.conn.Session("S1").EvalIsolated(ctxT(t), "F1", "1+2", nil); err != nil {
		t.Fatalf("EvalIsolated without a result: %v", err)
	}
}

func TestEvalIsolatedFailures(t *testing.T) {
	t.Parallel()

	t.Run("no context", func(t *testing.T) {
		t.Parallel()
		f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
			f.reply(cmd.ID, map[string]any{})
		})
		err := f.conn.EvalIsolated(ctxT(t), "S1", "F1", "1", nil)
		if err == nil || !strings.Contains(err.Error(), "no context") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("world refused", func(t *testing.T) {
		t.Parallel()
		f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
			f.replyError(cmd.ID, -32000, "No frame for given id found")
		})
		var pe *Error
		if err := f.conn.EvalIsolated(ctxT(t), "S1", "bad", "1", nil); !errors.As(err, &pe) {
			t.Fatalf("err = %v, want *Error", err)
		}
	})

	t.Run("exception", func(t *testing.T) {
		t.Parallel()
		f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
			if cmd.Method == "Page.createIsolatedWorld" {
				f.reply(cmd.ID, map[string]any{"executionContextId": 1})
				return
			}
			f.reply(cmd.ID, map[string]any{
				"result": map[string]any{"type": "object"},
				"exceptionDetails": map[string]any{
					"text":      "Uncaught",
					"exception": map[string]any{"description": "ReferenceError: nope is not defined"},
				},
			})
		})
		err := f.conn.EvalIsolated(ctxT(t), "S1", "F1", "nope", nil)
		if err == nil || !strings.Contains(err.Error(), "ReferenceError") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("exception without a description", func(t *testing.T) {
		t.Parallel()
		f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
			if cmd.Method == "Page.createIsolatedWorld" {
				f.reply(cmd.ID, map[string]any{"executionContextId": 1})
				return
			}
			f.reply(cmd.ID, map[string]any{
				"exceptionDetails": map[string]any{"text": "Uncaught SyntaxError"},
			})
		})
		err := f.conn.EvalIsolated(ctxT(t), "S1", "F1", "(", nil)
		if err == nil || !strings.Contains(err.Error(), "SyntaxError") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("undecodable value", func(t *testing.T) {
		t.Parallel()
		f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
			if cmd.Method == "Page.createIsolatedWorld" {
				f.reply(cmd.ID, map[string]any{"executionContextId": 1})
				return
			}
			f.reply(cmd.ID, map[string]any{"result": map[string]any{"value": "a string"}})
		})
		var n int
		err := f.conn.EvalIsolated(ctxT(t), "S1", "F1", "'a string'", &n)
		if err == nil || !strings.Contains(err.Error(), "evaluate result") {
			t.Fatalf("err = %v", err)
		}
	})
}
