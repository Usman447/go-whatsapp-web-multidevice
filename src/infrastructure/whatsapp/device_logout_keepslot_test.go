package whatsapp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

// keepSlotStubStorage is a minimal IChatStorageRepository that records the device
// registry calls exercised by the logout/purge paths and can be told to fail, so the
// tests can assert error propagation. All other interface methods are nil (embedded)
// and must not be called by the code under test.
type keepSlotStubStorage struct {
	domainChatStorage.IChatStorageRepository
	saveErr        error
	deleteDataErr  error
	savedRecords   []*domainChatStorage.DeviceRecord
	deletedData    []string
	deletedRecords []string
}

func (s *keepSlotStubStorage) SaveDeviceRecord(rec *domainChatStorage.DeviceRecord) error {
	cloned := *rec
	s.savedRecords = append(s.savedRecords, &cloned)
	return s.saveErr
}

func (s *keepSlotStubStorage) DeleteDeviceData(deviceID string) error {
	s.deletedData = append(s.deletedData, deviceID)
	return s.deleteDataErr
}

func (s *keepSlotStubStorage) DeleteDeviceRecord(deviceID string) error {
	s.deletedRecords = append(s.deletedRecords, deviceID)
	return nil
}

// The purge path drops the device's Chatwoot config with it; these slots have
// none, so the lookup returns empty and the delete methods are never reached.
func (s *keepSlotStubStorage) GetChatwootDeviceConfig(string) (*domainChatStorage.ChatwootDeviceConfig, error) {
	return nil, nil
}

// assertStoreLacksJID fails if any device row in the container still matches the given
// NonAD JID. Matching mirrors deleteStoreRowsForJID / LoadExistingDevices.
func assertStoreLacksJID(t *testing.T, ctx context.Context, c *sqlstore.Container, nonADJID string) {
	t.Helper()
	devices, err := c.GetAllDevices(ctx)
	if err != nil {
		t.Fatalf("get all devices: %v", err)
	}
	for _, d := range devices {
		if d != nil && d.ID != nil && d.ID.ToNonAD().String() == nonADJID {
			t.Fatalf("expected store to no longer contain jid %s", nonADJID)
		}
	}
}

// useFastLogoutHooks makes unlink retries fail immediately instead of dialing WhatsApp.
func useFastLogoutHooks(t *testing.T) {
	t.Helper()
	prevAttempts := logoutMaxAttempts
	prevDelay := logoutRetryDelay
	prevConnect := logoutConnect
	logoutMaxAttempts = 1
	logoutRetryDelay = 0
	logoutConnect = func(context.Context, *whatsmeow.Client) error {
		return errors.New("offline")
	}
	t.Cleanup(func() {
		logoutMaxAttempts = prevAttempts
		logoutRetryDelay = prevDelay
		logoutConnect = prevConnect
	})
}

// A stored session with no live client must not be deleted when the unlink cannot
// be sent. The rows stay so a later logout can still ask WhatsApp to remove the
// companion device. The slot id is deliberately different from the JID.
func TestLogoutDeviceKeepSlot_NoClientKeepsStoreRowsWhenUnlinkFails(t *testing.T) {
	useFastLogoutHooks(t)
	ctx := context.Background()
	primaryStore := newTestSQLStore(t)
	keysStore := newTestSQLStore(t)

	adJID := types.NewADJID("6281999999991", types.WhatsAppDomain, 10)
	nonAD := adJID.ToNonAD().String()
	if err := newTestStoreDevice(primaryStore, adJID, "primary").Save(ctx); err != nil {
		t.Fatalf("save primary device: %v", err)
	}
	if err := newTestStoreDevice(keysStore, adJID, "keys").Save(ctx); err != nil {
		t.Fatalf("save keys device: %v", err)
	}

	storage := &keepSlotStubStorage{}
	manager := NewDeviceManager(primaryStore, keysStore, storage)

	const slotID = "slot-uuid-1" // intentionally != JID
	inst := &DeviceInstance{id: slotID, jid: nonAD, displayName: "tIAtendo", createdAt: time.Now()}
	manager.devices[slotID] = inst

	err := manager.LogoutDeviceKeepSlot(ctx, slotID)
	if err == nil || !strings.Contains(err.Error(), "could not unlink") {
		t.Fatalf("expected unlink failure, got %v", err)
	}

	devices, listErr := primaryStore.GetAllDevices(ctx)
	if listErr != nil {
		t.Fatalf("get all devices: %v", listErr)
	}
	found := false
	for _, dev := range devices {
		if dev != nil && dev.ID != nil && dev.ID.ToNonAD().String() == nonAD {
			found = true
		}
	}
	if !found {
		t.Fatal("expected stored session to be kept after a failed unlink")
	}

	if _, ok := manager.GetDevice(slotID); !ok {
		t.Fatal("expected slot to be kept after failed logout")
	}
	if got := inst.JID(); got == "" {
		t.Fatal("expected instance JID to stay set so a later logout can unlink")
	}
	if len(storage.savedRecords) != 0 {
		t.Fatal("expected no slot reset when the unlink fails")
	}
}

// Scenario: DELETE purge removes the whatsmeow rows by JID from both containers even
// when the slot id differs from the JID (the old code compared the AD JID string to the
// slot id and never matched, so the rows were never deleted), and removes the slot.
func TestPurgeDevice_DeletesStoreRowsByJIDAcrossPrimaryAndKeys(t *testing.T) {
	ctx := context.Background()
	primaryStore := newTestSQLStore(t)
	keysStore := newTestSQLStore(t)

	adJID := types.NewADJID("6281999999992", types.WhatsAppDomain, 11)
	nonAD := adJID.ToNonAD().String()
	if err := newTestStoreDevice(primaryStore, adJID, "primary").Save(ctx); err != nil {
		t.Fatalf("save primary device: %v", err)
	}
	if err := newTestStoreDevice(keysStore, adJID, "keys").Save(ctx); err != nil {
		t.Fatalf("save keys device: %v", err)
	}

	storage := &keepSlotStubStorage{}
	manager := NewDeviceManager(primaryStore, keysStore, storage)

	const slotID = "slot-uuid-2"
	manager.devices[slotID] = &DeviceInstance{id: slotID, jid: nonAD, displayName: "tIAtendo", createdAt: time.Now()}

	if err := manager.PurgeDevice(ctx, slotID); err != nil {
		t.Fatalf("PurgeDevice returned error: %v", err)
	}

	assertStoreLacksJID(t, ctx, primaryStore, nonAD)
	assertStoreLacksJID(t, ctx, keysStore, nonAD)

	if _, ok := manager.GetDevice(slotID); ok {
		t.Fatal("expected slot to be removed after purge")
	}
	if len(storage.deletedData) == 0 || storage.deletedData[0] != slotID {
		t.Fatalf("expected chatstorage data deletion for %s, got %v", slotID, storage.deletedData)
	}
	if len(storage.deletedRecords) == 0 || storage.deletedRecords[0] != slotID {
		t.Fatalf("expected device record deletion for %s, got %v", slotID, storage.deletedRecords)
	}
}

// resetDeviceKeepSlot must surface a persistence failure instead of silently
// succeeding (coderabbitai device_manager.go:252-276), and LogoutDeviceKeepSlot must
// combine that error into its return value.
func TestResetDeviceKeepSlot_PropagatesSaveError(t *testing.T) {
	ctx := context.Background()
	storage := &keepSlotStubStorage{saveErr: errors.New("disk full")}
	manager := NewDeviceManager(nil, nil, storage)

	const slotID = "slot-persist-err"
	manager.devices[slotID] = &DeviceInstance{id: slotID, jid: "6281999999993@s.whatsapp.net", createdAt: time.Now()}

	if err := manager.resetDeviceKeepSlot(slotID); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected resetDeviceKeepSlot to propagate save error, got %v", err)
	}

	if err := manager.LogoutDeviceKeepSlot(ctx, slotID); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected LogoutDeviceKeepSlot to surface the persistence error, got %v", err)
	}
}

// DELETE promises a real purge: a LOCAL cleanup failure (chatstorage/store/keys) must
// be surfaced, not masked as success (aldinokemal/coderabbitai usecase device.go:68).
func TestPurgeDevice_SurfacesLocalCleanupFailure(t *testing.T) {
	ctx := context.Background()
	storage := &keepSlotStubStorage{deleteDataErr: errors.New("chatstorage down")}
	manager := NewDeviceManager(nil, nil, storage)

	const slotID = "slot-cleanup-err"
	manager.devices[slotID] = &DeviceInstance{id: slotID, createdAt: time.Now()}

	if err := manager.PurgeDevice(ctx, slotID); err == nil || !strings.Contains(err.Error(), "chatstorage down") {
		t.Fatalf("expected PurgeDevice to surface the local cleanup failure, got %v", err)
	}
}

// deleteStoreRowsForJID is a no-op for an empty JID: a slot that was never paired has no
// store rows, and an empty JID must never scan/delete anything.
func TestDeleteStoreRowsForJID_EmptyJIDIsNoOp(t *testing.T) {
	ctx := context.Background()
	primaryStore := newTestSQLStore(t)
	adJID := types.NewADJID("6281999999994", types.WhatsAppDomain, 12)
	if err := newTestStoreDevice(primaryStore, adJID, "primary").Save(ctx); err != nil {
		t.Fatalf("save primary device: %v", err)
	}

	manager := NewDeviceManager(primaryStore, nil, nil)
	if err := manager.deleteStoreRowsForJID(ctx, ""); err != nil {
		t.Fatalf("expected empty-jid no-op, got error: %v", err)
	}

	devices, err := primaryStore.GetAllDevices(ctx)
	if err != nil {
		t.Fatalf("get all devices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected the unrelated device to be retained, got %d devices", len(devices))
	}
}

// Scenario: the remote-logout callback holds a stale device id. InitWaCLI registers the
// callback with the instance's AD JID string (device.ID.String()), but loadFromRegistry
// can replace that instance with a named registry slot keyed by uuid (same account,
// NonAD JID). keepSlotLogout must then fall back to resolving the slot by JID, so the
// cleanup still deletes the store rows and clears the slot instead of no-opping with
// "device not found" (leaving a stale JID + orphan keys row).
func TestKeepSlotLogout_StaleADJIDFallsBackToJIDResolution(t *testing.T) {
	ctx := context.Background()
	primaryStore := newTestSQLStore(t)
	keysStore := newTestSQLStore(t)

	adJID := types.NewADJID("6281999999996", types.WhatsAppDomain, 14)
	nonAD := adJID.ToNonAD().String()
	if err := newTestStoreDevice(primaryStore, adJID, "primary").Save(ctx); err != nil {
		t.Fatalf("save primary device: %v", err)
	}
	if err := newTestStoreDevice(keysStore, adJID, "keys").Save(ctx); err != nil {
		t.Fatalf("save keys device: %v", err)
	}

	storage := &keepSlotStubStorage{}
	manager := NewDeviceManager(primaryStore, keysStore, storage)

	// The registry slot that replaced the startup instance: keyed by uuid, NonAD JID.
	const slotID = "slot-uuid-3"
	inst := &DeviceInstance{id: slotID, jid: nonAD, displayName: "tIAtendo", createdAt: time.Now()}
	manager.devices[slotID] = inst

	// The stale id the startup path registered the callback with: the AD JID string.
	if err := manager.keepSlotLogout(ctx, adJID.String()); err != nil {
		t.Fatalf("keepSlotLogout with stale AD JID id returned error: %v", err)
	}

	assertStoreLacksJID(t, ctx, primaryStore, nonAD)
	assertStoreLacksJID(t, ctx, keysStore, nonAD)

	if _, ok := manager.GetDevice(slotID); !ok {
		t.Fatal("expected slot to be kept after stale-id logout")
	}
	if got := inst.JID(); got != "" {
		t.Fatalf("expected instance JID cleared after stale-id logout, got %q", got)
	}
	if len(storage.savedRecords) == 0 {
		t.Fatal("expected SaveDeviceRecord to persist the kept slot")
	}
	last := storage.savedRecords[len(storage.savedRecords)-1]
	if last.DeviceID != slotID || last.JID != "" {
		t.Fatalf("expected persisted slot %s with empty JID, got %s / %q", slotID, last.DeviceID, last.JID)
	}
}

// Explicit logout for a paired client that is currently disconnected must still
// TRY the unlink. When that fails, the stored session stays so a later logout
// can send remove-companion-device. Deleting the rows here would make that
// retry impossible.
func TestLogoutDeviceKeepSlot_AttemptsUnlinkWhenPairedButDisconnected(t *testing.T) {
	useFastLogoutHooks(t)
	ctx := context.Background()
	primaryStore := newTestSQLStore(t)

	adJID := types.NewADJID("6281999999997", types.WhatsAppDomain, 15)
	nonAD := adJID.ToNonAD().String()
	dev := newTestStoreDevice(primaryStore, adJID, "primary")
	if err := dev.Save(ctx); err != nil {
		t.Fatalf("save primary device: %v", err)
	}

	// Real paired client (Store.ID set) that is not connected.
	cli := whatsmeow.NewClient(dev, nil)

	storage := &keepSlotStubStorage{}
	manager := NewDeviceManager(primaryStore, nil, storage)

	const slotID = "slot-uuid-4"
	inst := &DeviceInstance{id: slotID, jid: nonAD, displayName: "tIAtendo", client: cli, createdAt: time.Now()}
	manager.devices[slotID] = inst

	hook := logrustest.NewLocal(logrus.StandardLogger())
	defer hook.Reset()

	err := manager.LogoutDeviceKeepSlot(ctx, slotID)
	if err == nil || !strings.Contains(err.Error(), "could not unlink") {
		t.Fatalf("expected unlink failure, got %v", err)
	}

	attempted := false
	kept := false
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "remote unlink failed") {
			attempted = true
		}
		if strings.Contains(entry.Message, "local session kept") {
			kept = true
		}
	}
	if !attempted {
		t.Fatal("expected cli.Logout to be attempted for a paired-but-disconnected client")
	}
	if !kept {
		t.Fatal("expected local session to be kept after the failed unlink")
	}

	devices, listErr := primaryStore.GetAllDevices(ctx)
	if listErr != nil {
		t.Fatalf("get all devices: %v", listErr)
	}
	found := false
	for _, row := range devices {
		if row != nil && row.ID != nil && row.ID.ToNonAD().String() == nonAD {
			found = true
		}
	}
	if !found {
		t.Fatal("expected stored session to remain after a failed unlink")
	}
	if _, ok := manager.GetDevice(slotID); !ok {
		t.Fatal("expected slot to be kept after failed logout")
	}
	if got := inst.JID(); got == "" {
		t.Fatal("expected instance JID to stay set after a failed unlink")
	}
}

// Remote logout (events.LoggedOut) keeps the slot: routing the SetOnLoggedOut callback
// through keepSlotLogout (the same behavior both the lazy EnsureClient path and the
// startup InitWaCLI path now use) deletes the whatsmeow rows by JID but preserves the
// slot, instead of deleting it via RemoveDevice (aldinokemal device_manager.go:581).
func TestRemoteLogoutCallback_KeepsSlot(t *testing.T) {
	ctx := context.Background()
	primaryStore := newTestSQLStore(t)
	keysStore := newTestSQLStore(t)

	adJID := types.NewADJID("6281999999995", types.WhatsAppDomain, 13)
	nonAD := adJID.ToNonAD().String()
	if err := newTestStoreDevice(primaryStore, adJID, "primary").Save(ctx); err != nil {
		t.Fatalf("save primary device: %v", err)
	}
	if err := newTestStoreDevice(keysStore, adJID, "keys").Save(ctx); err != nil {
		t.Fatalf("save keys device: %v", err)
	}

	storage := &keepSlotStubStorage{}
	manager := NewDeviceManager(primaryStore, keysStore, storage)

	inst := &DeviceInstance{id: nonAD, jid: nonAD, displayName: "tIAtendo", createdAt: time.Now()}
	manager.devices[nonAD] = inst

	// Wire the callback exactly as the manager does for remote logout.
	inst.SetOnLoggedOut(func(deviceID string) {
		if err := manager.keepSlotLogout(context.Background(), deviceID); err != nil {
			t.Errorf("keepSlotLogout in remote-logout callback: %v", err)
		}
	})

	inst.TriggerLoggedOut()

	if _, ok := manager.GetDevice(nonAD); !ok {
		t.Fatal("expected slot to be kept on remote logout, but it was removed")
	}
	assertStoreLacksJID(t, ctx, primaryStore, nonAD)
	assertStoreLacksJID(t, ctx, keysStore, nonAD)
}
