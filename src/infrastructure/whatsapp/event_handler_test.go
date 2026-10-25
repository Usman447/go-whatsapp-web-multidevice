package whatsapp

import (
	"context"
	"testing"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	domainDevice "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/device"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestResolvePresenceOnConnect(t *testing.T) {
	tests := []struct {
		name      string
		configVal string
		wantPres  types.Presence
		wantSkip  bool
	}{
		{
			name:      "unavailable returns PresenceUnavailable",
			configVal: "unavailable",
			wantPres:  types.PresenceUnavailable,
			wantSkip:  false,
		},
		{
			name:      "available returns PresenceAvailable",
			configVal: "available",
			wantPres:  types.PresenceAvailable,
			wantSkip:  false,
		},
		{
			name:      "none skips sending presence",
			configVal: "none",
			wantPres:  "",
			wantSkip:  true,
		},
		{
			name:      "empty string defaults to unavailable",
			configVal: "",
			wantPres:  types.PresenceUnavailable,
			wantSkip:  false,
		},
		{
			name:      "unknown value defaults to unavailable",
			configVal: "garbage",
			wantPres:  types.PresenceUnavailable,
			wantSkip:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origVal := config.WhatsappPresenceOnConnect
			config.WhatsappPresenceOnConnect = tt.configVal
			defer func() { config.WhatsappPresenceOnConnect = origVal }()

			pres, skip := resolvePresenceOnConnect()
			if skip != tt.wantSkip {
				t.Errorf("resolvePresenceOnConnect() skip = %v, want %v", skip, tt.wantSkip)
			}
			if !skip && pres != tt.wantPres {
				t.Errorf("resolvePresenceOnConnect() presence = %q, want %q", pres, tt.wantPres)
			}
		})
	}
}

// FlexBase gym hosts recover a replaced session by exiting. systemd and WinSW
// restart the process, and startup reconnects the saved login. Staying up would
// leave the device offline because whatsmeow does not reconnect after this event.
func TestHandlerStreamReplacedExitsProcess(t *testing.T) {
	replaced := NewDeviceInstance("test-stream-replaced", nil, nil)
	replaced.SetState(domainDevice.DeviceStateLoggedIn)
	other := NewDeviceInstance("test-stream-other", nil, nil)
	other.SetState(domainDevice.DeviceStateLoggedIn)

	var got *int
	orig := exitProcess
	exitProcess = func(code int) {
		c := code
		got = &c
	}
	t.Cleanup(func() { exitProcess = orig })

	handler(context.Background(), replaced, &events.StreamReplaced{})

	if got == nil || *got != 0 {
		t.Fatalf("exit code = %v, want 0", got)
	}
	if state := other.State(); state != domainDevice.DeviceStateLoggedIn {
		t.Errorf("other device state = %s, want %s", state, domainDevice.DeviceStateLoggedIn)
	}
}
