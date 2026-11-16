package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureOK = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /widgets:
    get:
      tags: [widgets]
      operationId: listWidgets
      x-scope: client
      description: List every widget.
      responses:
        '200': { description: OK, content: { application/json: { schema: { type: string } } } }
    post:
      tags: [widgets]
      operationId: createWidget
      x-scope: admin
      description: |
        Creates a widget.

        Extra paragraph that must not leak into the summary.
      responses:
        '201': { description: Created }
  /widgets/{id}:
    parameters: [{ name: id, in: path, required: true, schema: { type: string } }]
    delete:
      tags: [widgets]
      operationId: deleteWidget
      x-scope: admin
      responses:
        '204': { description: Deleted }
components:
  schemas:
    Widget:
      type: object
      properties:
        description: { type: string, description: 'must not be read as an operation field' }
`

const fixtureMissingScope = `openapi: 3.1.0
paths:
  /widgets:
    get:
      tags: [widgets]
      operationId: listWidgets
      responses:
        '200': { description: OK }
components: {}
`

const fixtureMissingOperationID = `openapi: 3.1.0
paths:
  /widgets:
    get:
      tags: [widgets]
      x-scope: client
      responses:
        '200': { description: OK }
components: {}
`

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanOpenAPIOK(t *testing.T) {
	ops, err := scanOpenAPI(writeFixture(t, fixtureOK))
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("got %d operations, want 3: %+v", len(ops), ops)
	}

	byID := map[string]operation{}
	for _, op := range ops {
		byID[op.operationID] = op
	}

	list, ok := byID["listWidgets"]
	if !ok || list.method != "GET" || list.path != "/widgets" || list.scope != "client" {
		t.Errorf("listWidgets = %+v", list)
	}
	if list.summary != "List every widget." {
		t.Errorf("listWidgets.summary = %q", list.summary)
	}

	create, ok := byID["createWidget"]
	if !ok || create.summary != "Creates a widget." {
		t.Errorf("createWidget.summary = %q, want only the first line of the block scalar", create.summary)
	}

	del, ok := byID["deleteWidget"]
	if !ok || del.scope != "admin" {
		t.Errorf("deleteWidget = %+v", del)
	}
}

func TestScanOpenAPIFailsLoudlyOnMissingScope(t *testing.T) {
	_, err := scanOpenAPI(writeFixture(t, fixtureMissingScope))
	if err == nil {
		t.Fatal("want an error for an operation missing x-scope")
	}
	if !strings.Contains(err.Error(), "x-scope") {
		t.Errorf("error = %v, want it to mention x-scope", err)
	}
}

func TestScanOpenAPIFailsLoudlyOnMissingOperationID(t *testing.T) {
	_, err := scanOpenAPI(writeFixture(t, fixtureMissingOperationID))
	if err == nil {
		t.Fatal("want an error for an operation missing operationId")
	}
	if !strings.Contains(err.Error(), "operationId") {
		t.Errorf("error = %v, want it to mention operationId", err)
	}
}

func TestScanOpenAPIRealSpec(t *testing.T) {
	ops, err := scanOpenAPI(inRepo("api/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) < 20 {
		t.Errorf("got %d operations from the real spec, expected at least 20", len(ops))
	}
	for _, op := range ops {
		if op.operationID == "" || op.scope == "" {
			t.Errorf("operation %s %s missing operationId or x-scope", op.method, op.path)
		}
	}
}

func TestRenderAPIMDDeterministic(t *testing.T) {
	a, err := renderAPIMD()
	if err != nil {
		t.Fatal(err)
	}
	b, err := renderAPIMD()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("renderAPIMD is not deterministic across two runs")
	}
}
