package whatsapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"

	"holaclaw/internal/channel"
)

// Link starts a pairing-code login (§5.4): it connects the fresh device Run
// holds, waits for the first QR event (the socket is up; the QR itself is
// ignored) and asks WhatsApp for a code for phone, with a notification on the
// phone. The code comes back with an expiry; PairSuccess and the Connected
// that follows finish the link, which LinkState reports. An expired code, a
// PairError or a refused number fails it and deletes the half-made device.
// The code is returned here and through LinkState only, never logged.
func (a *Adapter) Link(ctx context.Context, phone string) (channel.LinkState, error) {
	digits, err := normalizePhone(phone)
	if err != nil {
		return channel.LinkState{}, err
	}
	a.sessionMu.Lock()
	a.stateMu.Lock()
	cli, gen, runCtx := a.client, a.clientGen, a.runCtx
	var refuse error
	switch {
	case cli == nil:
		refuse = conflict(fmt.Sprintf("whatsapp: channel %s is not running", a.name))
	case a.link.Status == channel.LinkPending:
		refuse = conflict("whatsapp: a link is already waiting for the phone")
	}
	if refuse == nil {
		if pn, _ := cli.OwnID(); !pn.IsEmpty() {
			refuse = conflict(fmt.Sprintf("whatsapp: already linked as +%s; run `%s` first", pn.User, cliLine("logout", a.name)))
		}
	}
	if refuse != nil {
		a.stateMu.Unlock()
		a.sessionMu.Unlock()
		return channel.LinkState{}, refuse
	}
	qr, end := make(chan struct{}), make(chan struct{})
	a.firstQR, a.linkEnd = qr, end
	a.link = channel.LinkState{Status: channel.LinkPending, Detail: detailLinking}
	a.stateMu.Unlock()
	// The wait for the socket runs without sessionMu, so Run and Unlink are
	// never stalled behind it; ending the link closes end, which stops the
	// wait.
	a.sessionMu.Unlock()
	a.report(channel.StateLinking, detailLinking)

	code, err := a.requestCode(ctx, runCtx, cli, qr, end, digits)
	if errors.Is(err, errLinkEnded) {
		st := a.LinkState()
		return st, fmt.Errorf("whatsapp: %s", cmp.Or(st.Detail, "the link was cancelled"))
	}
	if err != nil {
		a.failLink(gen, err.Error())
		return a.LinkState(), fmt.Errorf("whatsapp: %w", err)
	}
	a.stateMu.Lock()
	if gen == a.clientGen && a.link.Status == channel.LinkPending {
		a.link = channel.LinkState{Status: channel.LinkPending, Code: code, Expires: a.now().Add(codeTTL), Detail: detailLinking}
	}
	st := a.link
	a.stateMu.Unlock()
	go a.expire(runCtx, gen)
	return st, nil
}

// errLinkEnded is requestCode giving up because the link ended under it.
var errLinkEnded = errors.New("the link ended")

// linkErr pairs a human-readable message, shown to the operator verbatim,
// with the channel.Err* sentinel it classifies as (errors.Is). Its Error()
// leaves the message untouched; only Unwrap exposes the sentinel.
type linkErr struct {
	msg      string
	sentinel error
}

func (e linkErr) Error() string { return e.msg }
func (e linkErr) Unwrap() error { return e.sentinel }

// badPhone wraps a normalizePhone message, or WhatsApp refusing the number,
// as channel.ErrBadPhone.
func badPhone(msg string) error { return linkErr{msg, channel.ErrBadPhone} }

// conflict wraps a Link/Unlink state refusal as channel.ErrLinkConflict.
func conflict(msg string) error { return linkErr{msg, channel.ErrLinkConflict} }

// requestCode connects cli, waits for its first QR event and requests the
// pairing code. The socket lives on the run context, not the caller's: the
// request ends long before the phone answers. end closing stops every wait.
func (a *Adapter) requestCode(ctx, runCtx context.Context, cli waClient, qr, end <-chan struct{}, digits string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-end:
			cancel()
		case <-ctx.Done():
		}
	}()
	ended := func() bool {
		select {
		case <-end:
			return true
		default:
			return false
		}
	}
	if err := cli.Connect(runCtx); err != nil {
		if ended() {
			return "", errLinkEnded
		}
		return "", fmt.Errorf("connect: %w", err)
	}
	select {
	case <-qr:
	case <-end:
		return "", errLinkEnded
	case <-a.after(qrWait):
		return "", errors.New("WhatsApp did not open the linking connection in time")
	case <-ctx.Done():
		return "", ctx.Err()
	case <-runCtx.Done():
		return "", errors.New("the channel stopped")
	}
	code, err := cli.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	switch {
	case err != nil && ended():
		return "", errLinkEnded
	case errors.Is(err, whatsmeow.ErrIQBadRequest), errors.Is(err, whatsmeow.ErrIQNotFound):
		return "", badPhone(fmt.Sprintf("WhatsApp refused +%s: is the number on WhatsApp?", digits))
	case err != nil:
		return "", fmt.Errorf("request the code: %w", err)
	}
	return code, nil
}

// expire fails the link of generation gen once its code has lived codeTTL.
func (a *Adapter) expire(runCtx context.Context, gen int) {
	select {
	case <-a.after(codeTTL):
		a.enqueue(gen, linkExpired{})
	case <-runCtx.Done():
	}
}

// LinkState reports the current or last link.
func (a *Adapter) LinkState() channel.LinkState {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.link
}

// endLinkLocked records how a link ended and wakes a Link still waiting for
// the socket. The caller holds stateMu.
func (a *Adapter) endLinkLocked(st channel.LinkState) {
	a.link = st
	a.firstQR = nil
	if a.linkEnd != nil {
		close(a.linkEnd)
		a.linkEnd = nil
	}
}

// finishLink marks a pending link of generation gen done.
func (a *Adapter) finishLink(gen int, detail string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if gen == a.clientGen && a.link.Status == channel.LinkPending {
		a.endLinkLocked(channel.LinkState{Status: channel.LinkDone, Detail: detail})
	}
}

// failLink fails a pending link of generation gen with reason, deletes the
// device if pairing got as far as saving it, and opens a fresh one, back in
// unlinked.
func (a *Adapter) failLink(gen int, reason string) {
	a.stateMu.Lock()
	if gen != a.clientGen || a.link.Status != channel.LinkPending || a.client == nil {
		a.stateMu.Unlock()
		return
	}
	a.endLinkLocked(channel.LinkState{Status: channel.LinkFailed, Detail: reason})
	a.stateMu.Unlock()

	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	a.stateMu.Lock()
	cli, ctx, current := a.client, a.runCtx, gen == a.clientGen
	a.stateMu.Unlock()
	if !current || cli == nil {
		return // Unlink or a wipe replaced the client meanwhile
	}
	cli.Disconnect()
	// A device that never paired was never saved, and whatsmeow refuses to
	// delete one without an id.
	if pn, _ := cli.OwnID(); !pn.IsEmpty() {
		if err := cli.DeleteLocalDevice(ctx); err != nil {
			a.log.Warn("whatsapp half-made device not deleted", "channel", a.name, "error", err)
		}
	}
	if err := a.replace(ctx, false); err != nil {
		a.log.Error("whatsapp session reopen failed", "channel", a.name, "error", err)
	}
	a.report(channel.StateUnlinked, a.detailUnlinked())
}

// Unlink logs the device out, so the phone drops it from its linked devices,
// and deletes the session database. A pending link is cancelled. When
// WhatsApp cannot be told, the local session is deleted anyway and the error
// says to remove the device on the phone.
func (a *Adapter) Unlink(ctx context.Context) error {
	// A pending link ends first, without sessionMu, so a Link waiting for the
	// socket lets go at once.
	a.stateMu.Lock()
	cancelled := a.link.Status == channel.LinkPending
	if cancelled {
		a.endLinkLocked(channel.LinkState{Status: channel.LinkNone})
	}
	a.stateMu.Unlock()

	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	a.stateMu.Lock()
	cli, runCtx := a.client, a.runCtx
	a.stateMu.Unlock()
	if cli == nil {
		return conflict(fmt.Sprintf("whatsapp: channel %s is not running", a.name))
	}
	pn, _ := cli.OwnID()
	if pn.IsEmpty() && !cancelled {
		return conflict("whatsapp: not linked")
	}
	var logoutErr error
	if !pn.IsEmpty() {
		if err := cli.Logout(ctx); err != nil {
			logoutErr = fmt.Errorf("whatsapp: the session is deleted, but WhatsApp was not told (%w); remove the device in Linked devices on the phone", err)
		}
	}
	a.stateMu.Lock()
	a.endLinkLocked(channel.LinkState{Status: channel.LinkNone})
	a.stateMu.Unlock()
	if err := a.replace(runCtx, true); err != nil {
		return err
	}
	a.report(channel.StateUnlinked, a.detailUnlinked())
	return logoutErr
}

// normalizePhone turns a number in international format into its digits.
// Spaces, dashes and a leading "+" are ignored; a leading 0 means a national
// number, which WhatsApp cannot place.
func normalizePhone(s string) (string, error) {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == ' ' || r == '-':
		case r == '+' && i == 0:
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			return "", badPhone("whatsapp: the phone number takes digits only, in international format")
		}
	}
	d := b.String()
	switch {
	case d == "":
		return "", badPhone("whatsapp: a phone number is needed, in international format")
	case d[0] == '0':
		return "", badPhone("whatsapp: use the international format, with the country code")
	case len(d) < 7 || len(d) > 15: // E.164 holds at most 15 digits
		return "", badPhone("whatsapp: that is not a phone number in international format")
	}
	return d, nil
}
