// Package example carries the example config. `holaclaw init` writes its
// main agent, on the one model of the config.toml it writes itself.
package example

import "embed"

// FS is the example config directory: config.toml, channels.toml and the
// agents.
//
//go:embed config.toml channels.toml agents
var FS embed.FS
