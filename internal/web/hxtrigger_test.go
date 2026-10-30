package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// Why (#288): browsers read header bytes as Latin-1, so raw UTF-8 in HX-Trigger shows "KÃ¼che".
// The header must be pure ASCII (\uXXXX escapes, surrogate pairs above U+FFFF) and still decode,
// as JSON, to the same text.
func TestHXTriggerIsASCIIAndRoundTrips(t *testing.T) {
	const text = "Backup of Küche Ä 🙂 saved"
	w := httptest.NewRecorder()
	setHXTrigger(w, map[string]any{"toast": map[string]string{"text": text, "tone": "ok"}})
	h := w.Header().Get("HX-Trigger")
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			t.Fatalf("non-ASCII byte %#x at %d in %q", h[i], i, h)
		}
	}
	var got struct{ Toast struct{ Text, Tone string } }
	if err := json.Unmarshal([]byte(h), &got); err != nil || got.Toast.Text != text || got.Toast.Tone != "ok" {
		t.Fatalf("%q -> %+v, %v", h, got, err)
	}
}
