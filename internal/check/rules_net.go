package check

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"holaclaw/internal/config"
)

// net001Rule: binding a non-loopback address without TLS hands the control
// API to the network in the clear (docs/security.md: "binding non-loopback
// is refused without a token and TLS"), unless the operator acknowledged it
// with server.allow_insecure_transport.
func net001Rule() Rule {
	return Rule{
		ID:      "NET001",
		Summary: "server.listen is non-loopback without tls_cert and tls_key",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			s := in.Config.Server
			if s.Listen == "" || isLoopback(s.Listen) {
				return nil
			}
			if s.AllowInsecureTransport || (s.TLSCert != "" && s.TLSKey != "") {
				return nil
			}
			return []Finding{{
				ID:       "NET001",
				Severity: SeverityError,
				File:     config.FileConfig,
				Key:      "server.listen",
				Message:  fmt.Sprintf("listen %q is not loopback and no TLS certificate is configured", s.Listen),
				Fix:      "set server.tls_cert and server.tls_key, bind a loopback address, or acknowledge plain http with server.allow_insecure_transport",
			}}
		},
	}
}

// net002Rule: a configured TLS cert/key must actually be there — a typo here
// otherwise only shows up when the daemon fails to start.
func net002Rule() Rule {
	return Rule{
		ID:      "NET002",
		Summary: "tls_cert/tls_key set but the file is missing or unreadable",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			s := in.Config.Server
			var out []Finding
			for key, path := range map[string]string{"server.tls_cert": s.TLSCert, "server.tls_key": s.TLSKey} {
				if path == "" {
					continue
				}
				if _, err := os.Stat(path); err != nil {
					out = append(out, Finding{
						ID:       "NET002",
						Severity: SeverityError,
						File:     config.FileConfig,
						Key:      key,
						Message:  fmt.Sprintf("%s: %s", path, err),
						Fix:      "point " + key + " at a readable file, or create it",
					})
				}
			}
			return out
		},
	}
}

// isLoopback reports whether a "host:port" (or bare host) address resolves to
// the loopback interface. An address that cannot be classified — an unknown
// hostname, an empty host meaning "all interfaces" — is treated as
// non-loopback: the safer default for a security check.
func isLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// net003Rule: a provider that authenticates sends its key — and prompts, tool
// argument excerpts, image prompts — on every request. Over plain http to
// another machine that is all readable on the wire.
func net003Rule() Rule {
	return Rule{
		ID:      "NET003",
		Summary: "an authenticated provider is reached over plain http on a non-loopback host",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			report := func(key, raw string) {
				u, err := url.Parse(raw)
				if err != nil || u.Scheme != "http" || isLoopback(u.Host) {
					return
				}
				out = append(out, Finding{
					ID:       "NET003",
					Severity: SeverityError,
					File:     config.FileConfig,
					Key:      key,
					Message:  fmt.Sprintf("%q sends the provider's credentials in clear text", raw),
					Fix:      "use an https URL, or a loopback address",
				})
			}
			for _, name := range sortedKeys(in.Config.Providers) {
				p := in.Config.Providers[name]
				if p.Auth == "" || p.Auth == "none" {
					continue
				}
				report("providers."+name+".base_url", p.BaseURL)
				for _, mn := range sortedKeys(in.Config.Models) {
					if m := in.Config.Models[mn]; m.Provider == name && m.URL != "" {
						report("models."+mn+".url", m.URL)
					}
				}
			}
			return out
		},
	}
}
