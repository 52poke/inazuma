package mw

import (
	"testing"
	"time"
)

func TestClientTimeouts(t *testing.T) {
	if got, want := NewClient("http://mediawiki.test").http.Timeout, 10*time.Second; got != want {
		t.Errorf("foreground timeout = %s, want %s", got, want)
	}
	if got, want := NewClientWithTimeout("http://mediawiki.test", 40*time.Second).http.Timeout, 40*time.Second; got != want {
		t.Errorf("configured timeout = %s, want %s", got, want)
	}
}
