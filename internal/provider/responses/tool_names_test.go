package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

func TestToolAliasesUniqueAndStable(t *testing.T) {
	req := testRequest()
	for _, name := range []string{"files.read", "files_read", "files/read", "files_read_1", "", "mémoire", strings.Repeat("x", 65)} {
		req.Tools = append(req.Tools, provider.ToolSpec{Name: name})
	}
	names := toolNames(req)
	pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	seen := map[string]bool{}
	for original, alias := range names {
		if !pattern.MatchString(alias) || seen[alias] {
			t.Fatalf("invalid or duplicate alias for %q: %q", original, alias)
		}
		seen[alias] = true
	}
	if names["files_read"] != "files_read" || names["files_read_1"] != "files_read_1" {
		t.Fatal("valid names changed")
	}
	for i, j := 0, len(req.Tools)-1; i < j; i, j = i+1, j-1 {
		req.Tools[i], req.Tools[j] = req.Tools[j], req.Tools[i]
	}
	if !reflect.DeepEqual(names, toolNames(req)) {
		t.Fatal("aliases depend on tool order")
	}
}

func TestToolAliasRoundTrip(t *testing.T) {
	for _, returned := range []string{"files_read_1", "unadvertised"} {
		t.Run(returned, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body wireRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(body.Tools) != 2 || body.Tools[0].Name != "files_read_1" || body.Tools[1].Name != "files_read" {
					t.Errorf("unexpected tools: %+v", body.Tools)
				}
				if len(body.Input) != 2 || body.Input[0].Name != "files_read_1" || body.Input[1].CallID != "previous" {
					t.Errorf("unexpected history: %+v", body.Input)
				}
				sseHandler(t, fmt.Sprintf("event: response.output_item.done\ndata: {\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"next\",\"name\":%q,\"arguments\":\"{}\"}}\n\nevent: response.completed\ndata: {}\n\n", returned))(w, r)
			}), Options{})
			req := testRequest()
			req.Tools = []provider.ToolSpec{{Name: "files.read"}, {Name: "files_read"}}
			req.Messages = []provider.Message{
				{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "previous", Name: "files.read", Arguments: "{}"}}},
				{Role: provider.RoleTool, ToolCallID: "previous", Parts: []provider.Part{{Text: "ok"}}},
			}
			resp, _, err := chatOnce(t, c, req)
			if err != nil {
				t.Fatal(err)
			}
			want := returned
			if returned == "files_read_1" {
				want = "files.read"
			}
			if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != want {
				t.Fatalf("unexpected calls: %+v", resp.ToolCalls)
			}
			if req.Tools[0].Name != "files.read" || req.Messages[0].ToolCalls[0].Name != "files.read" {
				t.Fatal("request was mutated")
			}
		})
	}
}
