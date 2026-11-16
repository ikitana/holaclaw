package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestDeviceIntervalEncodings(t *testing.T) {
	for _, value := range []string{`5`, `"5"`} {
		var response deviceResponse
		if err := json.Unmarshal([]byte(`{"device_auth_id":"fake","interval":`+value+`}`), &response); err != nil {
			t.Fatal(err)
		}
		seconds, err := response.Interval.Int64()
		if err != nil || seconds != 5 || response.DeviceCode != "fake" {
			t.Fatalf("unexpected device response: %+v, %v", response, err)
		}
	}
}

func TestDevicePendingHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		f := newFake(t)
		f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
		f.device = jsonHandler(status, `{}`)
		p, _ := loginProvider(t, f, newClock())
		if _, err := p.Login().Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		done, err := p.Login().Poll(context.Background())
		if done || !errors.Is(err, ErrAuthorizationPending) || p.Login().State().Status != LoginPending {
			t.Fatalf("status %d: done=%v, err=%v, state=%v", status, done, err, p.Login().State().Status)
		}
	}
}

func TestDeviceStartNestedError(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error","code":"invalid_content_type","message":"Expected application/json"}}`)
	p, _ := loginProvider(t, f, newClock())
	_, err := p.Login().Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_content_type: Expected application/json") {
		t.Fatalf("error = %v", err)
	}
	if p.Login().State().Status != LoginFailed {
		t.Fatal("rejected device request must fail login")
	}
}

func TestDeviceCallbackURI(t *testing.T) {
	if DefaultRedirectURI != "https://auth.openai.com/deviceauth/callback" {
		t.Fatal("device login must use the device callback")
	}
}
