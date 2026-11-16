package api

import (
	"testing"

	"holaclaw/internal/api/gen"
)

func whatsappChannel(detail string, state gen.ChannelStatusState) gen.ChannelStatus {
	s := gen.ChannelStatus{Name: "wa", Type: "whatsapp", Agent: "main", State: state}
	if detail != "" {
		s.Detail = &detail
	}
	return s
}

// A WhatsApp invite opens a chat with the bot's number, the code typed.
func TestCreateInviteWhatsAppLink(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "wa", "whatsapp", "approved")
	name, uid := "wa", string(h.other.ID)

	channelsAre(h, whatsappChannel("+34600000000", gen.Connected))
	inv := createInvite(h, gen.InviteCreate{Channel: name, UserId: &uid})
	if inv.Link == nil {
		t.Fatal("no link on an invite for a connected whatsapp channel")
	}
	if want := "https://wa.me/34600000000?text=" + inv.Code; *inv.Link != want {
		t.Fatalf("link = %q, want %q", *inv.Link, want)
	}

	// Not connected, or a detail that is not a number: the code alone.
	for _, st := range []gen.ChannelStatus{
		whatsappChannel("run `holaclaw login whatsapp --name wa --phone <number>`", gen.Unlinked),
		whatsappChannel("+34600000000", gen.Reconnecting),
		whatsappChannel("WhatsApp logged this device out", gen.Connected),
	} {
		channelsAre(h, st)
		if inv := createInvite(h, gen.InviteCreate{Channel: name, UserId: &uid}); inv.Link != nil {
			t.Errorf("%s %q: link = %q, want none", st.State, *st.Detail, *inv.Link)
		}
	}

	// A room has no wa.me link, but the invite is allowed.
	channelsAre(h, whatsappChannel("+34600000000", gen.Connected))
	kind := gen.InviteCreateKindRoom
	if inv := createInvite(h, gen.InviteCreate{Channel: name, Kind: &kind}); inv.Link != nil || inv.Code == "" {
		t.Fatalf("room invite = %+v, want a code and no link", inv)
	}
}

func TestWhatsappInviteLink(t *testing.T) {
	cases := map[string]struct{ number, code, want string }{
		"plus":       {"+34600000000", "ab12", "https://wa.me/34600000000?text=ab12"},
		"no plus":    {"34600000000", "ab12", "https://wa.me/34600000000?text=ab12"},
		"escaped":    {"+34600000000", "a b&c", "https://wa.me/34600000000?text=a+b%26c"},
		"no code":    {"+34600000000", "", ""},
		"not digits": {"+34 600", "ab12", ""},
		"too short":  {"+123", "ab12", ""},
		"empty":      {"", "ab12", ""},
	}
	for name, tc := range cases {
		if got := whatsappInviteLink(tc.number, tc.code); got != tc.want {
			t.Errorf("%s: link = %q, want %q", name, got, tc.want)
		}
	}
}
