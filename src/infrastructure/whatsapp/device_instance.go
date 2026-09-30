package whatsapp

import (
	"context"
	"errors"
	"sync"
	"time"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	domainDevice "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/device"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// DeviceInstance bundles a WhatsApp client with device metadata and scoped storage.
type DeviceInstance struct {
	mu              sync.RWMutex
	id              string
	client          *whatsmeow.Client
	chatStorageRepo domainChatStorage.IChatStorageRepository
	state           domainDevice.DeviceState
	displayName     string
	phoneNumber     string
	jid             string // bare-number (NonAD) JID: chat storage / webhook partition key
	adJID           string // full AD JID (number:NN@s.whatsapp.net): pins the exact companion session
	createdAt       time.Time
	onLoggedOut     func(deviceID string) // Callback for remote logout cleanup

	// Pending passkey pairing state, populated by PairPasskey* events during login.
	passkeyChallenge     *types.WebAuthnPublicKey
	passkeyCode          string
	passkeySkipHandoffUX bool

	// Open QR login. A later Login() returns the latest image while qrActive is set
	// and must not Disconnect() the socket the phone may already be scanning.
	qrGen       int
	qrActive    bool
	qrImagePath string
	qrDuration  time.Duration
	qrReady     chan struct{}
	qrCancel    context.CancelFunc
}

// ErrQRLoginEnded is returned when a waiter arrives after the QR channel closed
// without a scannable image.
var ErrQRLoginEnded = errors.New("qr login ended")

func NewDeviceInstance(deviceID string, client *whatsmeow.Client, chatStorageRepo domainChatStorage.IChatStorageRepository) *DeviceInstance {
	jid := ""
	adJID := ""
	display := ""
	if client != nil && client.Store != nil && client.Store.ID != nil {
		jid = client.Store.ID.ToNonAD().String()
		adJID = client.Store.ID.String()
		display = client.Store.PushName
	}

	return &DeviceInstance{
		id:              deviceID,
		client:          client,
		chatStorageRepo: chatStorageRepo,
		state:           domainDevice.DeviceStateDisconnected,
		displayName:     display,
		jid:             jid,
		adJID:           adJID,
		createdAt:       time.Now(),
	}
}

func (d *DeviceInstance) ID() string {
	return d.id
}

func (d *DeviceInstance) GetClient() *whatsmeow.Client {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.client
}

func (d *DeviceInstance) GetChatStorage() domainChatStorage.IChatStorageRepository {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.chatStorageRepo
}

func (d *DeviceInstance) SetState(state domainDevice.DeviceState) {
	d.mu.Lock()
	d.state = state
	d.mu.Unlock()
}

func (d *DeviceInstance) State() domainDevice.DeviceState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

func (d *DeviceInstance) DisplayName() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.displayName
}

func (d *DeviceInstance) PhoneNumber() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.phoneNumber
}

func (d *DeviceInstance) JID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.jid
}

// ADJID returns the full companion identity (number:NN@s.whatsapp.net), or "" while
// the slot is unpaired / the suffix is not yet known.
func (d *DeviceInstance) ADJID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.adJID
}

func (d *DeviceInstance) CreatedAt() time.Time {
	return d.createdAt
}

// SetClient attaches a WhatsApp client to this instance and updates metadata.
func (d *DeviceInstance) SetClient(client *whatsmeow.Client) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.client = client
	d.refreshIdentityLocked()
	d.state = domainDevice.DeviceStateDisconnected
}

// ResetClient detaches the WhatsApp client and clears the session-derived identity
// (jid, phone number) so the slot can be re-paired with a fresh client on the next
// login. The device id, display name and creation time are preserved, keeping the
// slot in place after a logout.
func (d *DeviceInstance) ResetClient() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.client = nil
	d.jid = ""
	d.adJID = ""
	d.phoneNumber = ""
	d.state = domainDevice.DeviceStateDisconnected
	d.endQRLocked()
}

// StartQRLogin marks a new QR socket as in progress. The bool is false when a
// session is already open; the caller must reuse that session and must not
// disconnect it. gen identifies this session so a finished goroutine cannot
// clear a newer one.
func (d *DeviceInstance) StartQRLogin() (gen int, started bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.qrActive {
		return d.qrGen, false
	}
	d.qrGen++
	d.qrActive = true
	d.qrImagePath = ""
	d.qrDuration = 0
	d.qrReady = make(chan struct{})
	return d.qrGen, true
}

// UpdateQRLogin records the latest scannable image for this generation.
func (d *DeviceInstance) UpdateQRLogin(gen int, imagePath string, duration time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.qrActive || d.qrGen != gen {
		return
	}
	first := d.qrImagePath == ""
	d.qrImagePath = imagePath
	d.qrDuration = duration
	if first && d.qrReady != nil {
		close(d.qrReady)
	}
}

// CurrentQRLogin returns the latest image while the QR socket is still open.
func (d *DeviceInstance) CurrentQRLogin() (imagePath string, duration time.Duration, ok bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.qrActive || d.qrImagePath == "" {
		return "", 0, false
	}
	return d.qrImagePath, d.qrDuration, true
}

// QRLoginActive reports whether a QR socket is open, including before the first image.
func (d *DeviceInstance) QRLoginActive() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.qrActive
}

// WaitQRLogin blocks until the first image of the open session is available.
func (d *DeviceInstance) WaitQRLogin(ctx context.Context) (string, time.Duration, error) {
	if path, dur, ok := d.CurrentQRLogin(); ok {
		return path, dur, nil
	}
	d.mu.RLock()
	ready := d.qrReady
	active := d.qrActive
	d.mu.RUnlock()
	if !active || ready == nil {
		return "", 0, ErrQRLoginEnded
	}
	select {
	case <-ready:
		if path, dur, ok := d.CurrentQRLogin(); ok {
			return path, dur, nil
		}
		return "", 0, ErrQRLoginEnded
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
}

// SetQRCancel stores the QR context cancel for this generation so a later login
// can stop a socket whose image file is already gone.
func (d *DeviceInstance) SetQRCancel(gen int, cancel context.CancelFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.qrGen != gen || !d.qrActive {
		if cancel != nil {
			cancel()
		}
		return
	}
	d.qrCancel = cancel
}

// EndQRLogin closes the session for gen. A mismatched gen is ignored.
func (d *DeviceInstance) EndQRLogin(gen int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.qrGen != gen {
		return
	}
	d.endQRLocked()
}

// ClearQRLogin ends whatever QR session is open so the next login can start clean.
func (d *DeviceInstance) ClearQRLogin() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.qrGen++
	d.endQRLocked()
}

func (d *DeviceInstance) endQRLocked() {
	if d.qrCancel != nil {
		d.qrCancel()
		d.qrCancel = nil
	}
	d.qrActive = false
	d.qrImagePath = ""
	d.qrDuration = 0
	if d.qrReady != nil {
		select {
		case <-d.qrReady:
		default:
			close(d.qrReady)
		}
		d.qrReady = nil
	}
}

// SetChatStorage swaps the chat storage repository for this device.
func (d *DeviceInstance) SetChatStorage(repo domainChatStorage.IChatStorageRepository) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.chatStorageRepo = repo
}

// IsConnected returns the live connection flag if a client exists.
func (d *DeviceInstance) IsConnected() bool {
	d.mu.RLock()
	client := d.client
	d.mu.RUnlock()
	if client == nil {
		return false
	}
	return client.IsConnected()
}

// IsLoggedIn returns the login status if a client exists.
func (d *DeviceInstance) IsLoggedIn() bool {
	d.mu.RLock()
	client := d.client
	d.mu.RUnlock()
	if client == nil {
		return false
	}
	return client.IsLoggedIn()
}

// UpdateStateFromClient refreshes the snapshot state based on the client flags.
func (d *DeviceInstance) UpdateStateFromClient() domainDevice.DeviceState {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch {
	case d.client != nil && d.client.IsLoggedIn():
		d.state = domainDevice.DeviceStateLoggedIn
	case d.client != nil && d.client.IsConnected():
		d.state = domainDevice.DeviceStateConnected
	default:
		d.state = domainDevice.DeviceStateDisconnected
	}

	d.refreshIdentityLocked()
	return d.state
}

func (d *DeviceInstance) refreshIdentityLocked() {
	if d.client != nil && d.client.Store != nil && d.client.Store.ID != nil {
		d.jid = d.client.Store.ID.ToNonAD().String()
		d.adJID = d.client.Store.ID.String()
		d.displayName = d.client.Store.PushName
	}
}

// SetPasskeyChallenge stores a pending WebAuthn challenge and clears any previous confirmation code.
func (d *DeviceInstance) SetPasskeyChallenge(pk *types.WebAuthnPublicKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.passkeyChallenge = pk
	d.passkeyCode = ""
	d.passkeySkipHandoffUX = false
}

// SetPasskeyConfirmation stores the pairing confirmation code and clears the pending challenge.
func (d *DeviceInstance) SetPasskeyConfirmation(code string, skipHandoffUX bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.passkeyChallenge = nil
	d.passkeyCode = code
	d.passkeySkipHandoffUX = skipHandoffUX
}

// PasskeyState returns the pending challenge, confirmation code and skip-handoff flag.
func (d *DeviceInstance) PasskeyState() (*types.WebAuthnPublicKey, string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.passkeyChallenge, d.passkeyCode, d.passkeySkipHandoffUX
}

// ClearPasskeyState resets all pending passkey pairing state.
func (d *DeviceInstance) ClearPasskeyState() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.passkeyChallenge = nil
	d.passkeyCode = ""
	d.passkeySkipHandoffUX = false
}

func (d *DeviceInstance) SetOnLoggedOut(callback func(deviceID string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onLoggedOut = callback
}

func (d *DeviceInstance) TriggerLoggedOut() {
	d.mu.RLock()
	callback := d.onLoggedOut
	deviceID := d.id
	d.mu.RUnlock()

	if callback != nil {
		callback(deviceID)
	}
}
