// Package gen holds the server types generated from api/openapi.yaml.
// Regenerate with `just gen`; CI fails when the output is stale.
package gen

//go:generate oapi-codegen -config cfg.yaml ../../../api/openapi.yaml
