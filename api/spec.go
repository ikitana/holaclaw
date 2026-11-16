// Package api embeds the OpenAPI spec, the source of truth of the Control API.
package api

import _ "embed"

// Spec is api/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
