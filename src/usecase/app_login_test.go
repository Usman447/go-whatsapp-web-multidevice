package usecase

import (
	"testing"
	"time"
)

func TestShouldStartFreshQR(t *testing.T) {
	if shouldStartFreshQR(true, false) {
		t.Fatal("deleted store must be replaced before a new socket")
	}
	if shouldStartFreshQR(false, true) {
		t.Fatal("a live QR must be returned without Disconnect")
	}
	if shouldStartFreshQR(true, true) {
		t.Fatal("deleted store wins over a stale QR snapshot")
	}
	if !shouldStartFreshQR(false, false) {
		t.Fatal("no live QR and a usable store should start a socket")
	}
}

func TestQRCodeDurationKeepsMostOfTheCodeLifetime(t *testing.T) {
	if got := qrCodeDuration(60 * time.Second); got != 58 {
		t.Fatalf("60s code duration = %v, want 58", got)
	}
	if got := qrCodeDuration(20 * time.Second); got != 18 {
		t.Fatalf("20s code duration = %v, want 18", got)
	}
	if got := qrCodeDuration(10 * time.Second); got != 15 {
		t.Fatalf("short code duration = %v, want 15", got)
	}
}
