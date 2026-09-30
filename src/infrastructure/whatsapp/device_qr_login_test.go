package whatsapp

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

func TestCurrentQRLoginReusesOpenSession(t *testing.T) {
	inst := NewDeviceInstance("gymstation", nil, nil)

	gen, started := inst.StartQRLogin()
	if !started {
		t.Fatal("first StartQRLogin should own the socket")
	}
	if _, startedAgain := inst.StartQRLogin(); startedAgain {
		t.Fatal("second StartQRLogin must not open another socket")
	}

	inst.UpdateQRLogin(gen, "statics/qrcode/scan-qr-first.png", 58)
	path, dur, ok := inst.CurrentQRLogin()
	if !ok || path != "statics/qrcode/scan-qr-first.png" || dur != 58 {
		t.Fatalf("live QR = %q %v %v", path, dur, ok)
	}

	inst.UpdateQRLogin(gen, "statics/qrcode/scan-qr-rotated.png", 18)
	path, _, ok = inst.CurrentQRLogin()
	if !ok || path != "statics/qrcode/scan-qr-rotated.png" {
		t.Fatalf("rotated QR = %q ok=%v", path, ok)
	}

	inst.EndQRLogin(gen)
	if inst.QRLoginActive() {
		t.Fatal("ended session still active")
	}
	if _, _, ok := inst.CurrentQRLogin(); ok {
		t.Fatal("ended session still has an image")
	}
	if _, started := inst.StartQRLogin(); !started {
		t.Fatal("a new QR should be allowed after the channel ends")
	}
}

func TestStaleQRGenerationDoesNotClearNewerSession(t *testing.T) {
	inst := NewDeviceInstance("dev", nil, nil)
	oldGen, _ := inst.StartQRLogin()
	inst.UpdateQRLogin(oldGen, "old.png", 58)
	inst.EndQRLogin(oldGen)

	newGen, started := inst.StartQRLogin()
	if !started || newGen == oldGen {
		t.Fatalf("new gen %d started=%v old %d", newGen, started, oldGen)
	}
	inst.UpdateQRLogin(newGen, "new.png", 58)
	inst.UpdateQRLogin(oldGen, "stale.png", 1)
	inst.EndQRLogin(oldGen)

	path, _, ok := inst.CurrentQRLogin()
	if !ok || path != "new.png" {
		t.Fatalf("newer QR overwritten: %q ok=%v", path, ok)
	}
}

func TestWaitQRLoginUnblocksOnFirstImage(t *testing.T) {
	inst := NewDeviceInstance("dev-win", nil, nil)
	gen, _ := inst.StartQRLogin()

	done := make(chan struct{})
	var path string
	var err error
	go func() {
		defer close(done)
		path, _, err = inst.WaitQRLogin(context.Background())
	}()

	inst.UpdateQRLogin(gen, "waiting.png", 40)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitQRLogin did not return")
	}
	if err != nil || path != "waiting.png" {
		t.Fatalf("wait result path=%q err=%v", path, err)
	}
}

func TestReplaceDeletedClientDropsStoreAndKeepsSlot(t *testing.T) {
	mgr := NewDeviceManager(nil, nil, nil)
	client := whatsmeow.NewClient(&store.Device{Deleted: true}, nil)
	if !ClientStoreDeleted(client) {
		t.Fatal("expected deleted store")
	}
	inst := NewDeviceInstance("gymstation", client, nil)
	mgr.AddDevice(inst)

	if err := mgr.ReplaceDeletedClient("gymstation"); err != nil {
		t.Fatal(err)
	}
	if inst.ID() != "gymstation" {
		t.Fatalf("slot id changed: %s", inst.ID())
	}
	if inst.GetClient() != nil {
		t.Fatal("deleted client still attached; EnsureClient would reuse it")
	}
	if inst.JID() != "" {
		t.Fatalf("jid still set: %s", inst.JID())
	}
	if ClientStoreDeleted(inst.GetClient()) {
		t.Fatal("nil client reported as deleted")
	}
}
