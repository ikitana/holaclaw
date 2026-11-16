package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/config"
)

// The browser rules (docs/browser.md). Every one of them is about the
// deployment, not the config file: a sidecar that cannot start is the
// failure this milestone is most likely to meet on a real host, and each of
// these says the exact command that fixes it. They all skip silently when
// there is no [browser] table — no browser configured, nothing to check.

// browserDialTimeout bounds the BRW004 socket probe. The sidecar is on the
// same machine; half a second is generous.
const browserDialTimeout = 500 * time.Millisecond

// procRoot is /proc, replaced by the tests. The sandbox switches BRW003
// reads live there, and there is no other way to ask about them.
var procRoot = "/proc"

// browserOff reports whether the browser rules have nothing to look at.
func browserOff(in *Input) bool { return in.Config == nil || !in.Config.Browser.Enabled }

// brw001Rule: no usable browser binary. The sidecar downloads nothing, so a
// missing or ancient Chromium is a deployment step nobody took.
func brw001Rule() Rule {
	return Rule{
		ID:      "BRW001",
		Summary: "no usable browser binary for the sidecar",
		Run: func(_ context.Context, in *Input) []Finding {
			if browserOff(in) {
				return nil
			}
			finding := func(msg string) []Finding {
				return []Finding{{
					ID:       "BRW001",
					Severity: SeverityError,
					File:     config.FileConfig,
					Key:      "browser.binary",
					Message:  msg,
					Fix:      "install chromium (apt install chromium), or set browser.binary to the browser's path",
				}}
			}
			b, err := chromium.Find(in.Config.Browser.Binary)
			if err != nil {
				return finding(browserFindMessage(in.Config.Browser.Binary, err))
			}
			if b.TooOld() {
				return finding(fmt.Sprintf("%s is version %s; the sidecar needs major %d or newer",
					b.Path, b.Version, chromium.MinMajor))
			}
			return nil
		},
	}
}

// browserFindMessage turns a Find failure into one sentence. The error
// already names the path or the candidates it looked for.
func browserFindMessage(configured string, err error) string {
	if configured == "" && errors.Is(err, chromium.ErrNotFound) {
		return "no browser found on PATH (chromium, chromium-browser, chrome-for-testing, google-chrome)"
	}
	return strings.TrimPrefix(err.Error(), "chromium: ")
}

// brw002Rule: visible is the default (docs/browser.md) and a visible browser needs the
// virtual display the sidecar starts itself. Falling back to headless behind
// the operator's back would change what sites see, so this is an error.
func brw002Rule() Rule {
	return Rule{
		ID:      "BRW002",
		Summary: "headless is off but Xvfb is not installed",
		Run: func(_ context.Context, in *Input) []Finding {
			if browserOff(in) || in.Config.Browser.Headless {
				return nil
			}
			if _, err := exec.LookPath("Xvfb"); err == nil {
				return nil
			}
			return []Finding{{
				ID:       "BRW002",
				Severity: SeverityError,
				File:     config.FileConfig,
				Key:      "browser.headless",
				Message:  "the browser runs visible by default and Xvfb is not on PATH",
				Fix:      "install xvfb (apt install xvfb), or set browser.headless = true",
			}}
		},
	}
}

// browserSandboxSwitch is one kernel knob Chromium's sandbox needs, the value
// that breaks it, and the command that puts it right.
type browserSandboxSwitch struct {
	file string // relative to procRoot
	bad  string // the value that makes the sandbox unusable
	what string
	fix  string
}

// browserSandbox is what a host can have set against an unprivileged user
// namespace. --no-sandbox is never used (docs/browser.md): if these are wrong the sidecar
// refuses to run, so `check` has to say which one and how to undo it.
var browserSandbox = []browserSandboxSwitch{
	{
		file: "sys/kernel/unprivileged_userns_clone", bad: "0",
		what: "unprivileged user namespaces are disabled",
		fix:  "sysctl -w kernel.unprivileged_userns_clone=1 (and add it to /etc/sysctl.d/)",
	},
	{
		file: "sys/user/max_user_namespaces", bad: "0",
		what: "the user namespace limit is 0",
		fix:  "sysctl -w user.max_user_namespaces=15000 (and add it to /etc/sysctl.d/)",
	},
	{
		file: "sys/kernel/apparmor_restrict_unprivileged_userns", bad: "1",
		what: "AppArmor restricts unprivileged user namespaces",
		fix:  "install an AppArmor profile for the browser binary, or sysctl -w kernel.apparmor_restrict_unprivileged_userns=0",
	},
}

// brw003Rule: Chromium's own sandbox cannot start here. Each switch is read
// where it exists and skipped where it does not — the three are distro- and
// kernel-specific, and an absent file says nothing.
func brw003Rule() Rule {
	return Rule{
		ID:      "BRW003",
		Summary: "Chromium's sandbox cannot run on this host",
		Run: func(_ context.Context, in *Input) []Finding {
			if browserOff(in) {
				return nil
			}
			var out []Finding
			for _, s := range browserSandbox {
				p := filepath.Join(procRoot, s.file)
				raw, err := os.ReadFile(p) //nolint:gosec // a fixed path under procRoot
				if err != nil || strings.TrimSpace(string(raw)) != s.bad {
					continue
				}
				out = append(out, Finding{
					ID:       "BRW003",
					Severity: SeverityError,
					File:     p,
					Message:  s.what + ": Chromium's sandbox cannot start, and the sidecar never uses --no-sandbox",
					Fix:      s.fix,
				})
			}
			return out
		},
	}
}

// brw004Rule: the sidecar's socket does not answer. A warning, not an error:
// the unit may simply not be started yet, and the daemon runs fine without it.
func brw004Rule() Rule {
	return Rule{
		ID:      "BRW004",
		Summary: "the browser sidecar socket is not reachable",
		Run: func(_ context.Context, in *Input) []Finding {
			socket := ""
			if !browserOff(in) {
				socket = in.Config.Browser.Socket
			}
			if socket == "" || in.Offline {
				return nil
			}
			conn, err := net.DialTimeout("unix", socket, browserDialTimeout)
			if err == nil {
				_ = conn.Close()
				return nil
			}
			return []Finding{{
				ID:       "BRW004",
				Severity: SeverityWarning,
				File:     config.FileConfig,
				Key:      "browser.socket",
				Message:  fmt.Sprintf("nothing is listening on %s", socket),
				Fix:      "systemctl start holaclaw-browser",
			}}
		},
	}
}

// brw005Rule: an agent with the browser tool and no allow-list may open any
// site. That is a supported choice — the policy gate, the taint and the step
// approvals still apply — so it is a warning the operator can accept.
func brw005Rule() Rule {
	return Rule{
		ID:      "BRW005",
		Summary: "agent may browse any site: no browser.allow list",
		Run: func(_ context.Context, in *Input) []Finding {
			if browserOff(in) {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Agents) {
				a := in.Config.Agents[name]
				if len(a.Browser.Allow) > 0 {
					continue
				}
				if !contains(a.Tools, "browser") && !contains(a.Tools, "browser.do") {
					continue
				}
				out = append(out, Finding{
					ID:       "BRW005",
					Severity: SeverityWarning,
					File:     path.Join("agents", name, config.FileAgent),
					Key:      "browser.allow",
					Message:  fmt.Sprintf("agent %q may open any site with browser.do", name),
					Fix:      `add [browser] allow = ["example.com"] to agent.toml, or accept it`,
				})
			}
			return out
		},
	}
}

// brw006Rule: browser.do with no `decide` role means the chat model answers
// every narrow question — two extra chat calls per move, the thing the loop of
// docs/browser.md exists to avoid.
func brw006Rule() Rule {
	return Rule{
		ID:      "BRW006",
		Summary: "agent browses with the chat model as decider: no models.decide",
		Run: func(_ context.Context, in *Input) []Finding {
			if browserOff(in) {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Agents) {
				a := in.Config.Agents[name]
				if a.Models.Decide != "" {
					continue
				}
				if !contains(a.Tools, "browser") && !contains(a.Tools, "browser.do") {
					continue
				}
				out = append(out, Finding{
					ID:       "BRW006",
					Severity: SeverityWarning,
					File:     path.Join("agents", name, config.FileAgent),
					Key:      "models.decide",
					Message:  fmt.Sprintf("agent %q has browser.do but no decide role: the chat model answers every question the decision model would (slow: two extra chat calls per move)", name),
					Fix:      `set models.decide to a decision model (e.g. the guard's, decide = "jev") in agent.toml`,
				})
			}
			return out
		},
	}
}
