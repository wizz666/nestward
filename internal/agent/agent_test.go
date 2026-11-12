package agent

import (
	"context"
	"errors"
	"testing"
)

func TestLoadNotEnrolled(t *testing.T) {
	if _, _, err := Load(t.TempDir()); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("err = %v", err)
	}
}

func TestEnrollNeedsHTTPS(t *testing.T) {
	err := Enroll(context.Background(), t.TempDir(), "http://core:8472", "CODE", "sha256:x", "m", "h")
	if err == nil {
		t.Error("plain http must be refused")
	}
}
