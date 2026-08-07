package store

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestEncodeDecodeCursorRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 5, 12, 34, 56, 789000000, time.UTC)
	cursor := EncodeCursor(want, 42)

	gotTime, gotID, ok, err := DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeCursor(%q): unexpected error: %v", cursor, err)
	}
	if !ok {
		t.Fatalf("DecodeCursor(%q): ok = false, want true", cursor)
	}
	if !gotTime.Equal(want) {
		t.Errorf("DecodeCursor(%q): time = %v, want %v", cursor, gotTime, want)
	}
	if gotID != 42 {
		t.Errorf("DecodeCursor(%q): id = %d, want 42", cursor, gotID)
	}
}

func TestDecodeCursorEmptyMeansNoCursor(t *testing.T) {
	gotTime, gotID, ok, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("DecodeCursor(\"\"): unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("DecodeCursor(\"\"): ok = true, want false")
	}
	if !gotTime.IsZero() || gotID != 0 {
		t.Errorf("DecodeCursor(\"\"): got (%v, %d), want zero values", gotTime, gotID)
	}
}

func TestDecodeCursorMalformedBase64(t *testing.T) {
	_, _, ok, err := DecodeCursor("not-valid-base64!!!")
	if err == nil {
		t.Fatal("DecodeCursor(malformed base64): expected error, got nil")
	}
	if ok {
		t.Error("DecodeCursor(malformed base64): ok = true, want false")
	}
}

func TestDecodeCursorMissingSeparator(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("2026-08-05T12:34:56.789Z_no_separator_here"))
	_, _, ok, err := DecodeCursor(raw)
	if err == nil {
		t.Fatal("DecodeCursor(missing separator): expected error, got nil")
	}
	if ok {
		t.Error("DecodeCursor(missing separator): ok = true, want false")
	}
}

func TestDecodeCursorBadTimestamp(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("not-a-timestamp|42"))
	_, _, ok, err := DecodeCursor(raw)
	if err == nil {
		t.Fatal("DecodeCursor(bad timestamp): expected error, got nil")
	}
	if ok {
		t.Error("DecodeCursor(bad timestamp): ok = true, want false")
	}
}

func TestDecodeCursorNonNumericID(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("2026-08-05T12:34:56.789Z|not-a-number"))
	_, _, ok, err := DecodeCursor(raw)
	if err == nil {
		t.Fatal("DecodeCursor(non-numeric id): expected error, got nil")
	}
	if ok {
		t.Error("DecodeCursor(non-numeric id): ok = true, want false")
	}
}

func TestEncodeCursorIsStableAcrossCalls(t *testing.T) {
	ts := time.Date(2026, 8, 5, 12, 34, 56, 789000000, time.UTC)
	first := EncodeCursor(ts, 7)
	second := EncodeCursor(ts, 7)
	if first != second {
		t.Errorf("EncodeCursor is not stable: %q != %q", first, second)
	}
}

func TestDecodeCursorNeverPanics(t *testing.T) {
	inputs := []string{
		"",
		"!!!",
		"====",
		base64.RawURLEncoding.EncodeToString([]byte("|")),
		base64.RawURLEncoding.EncodeToString([]byte("||")),
		base64.RawURLEncoding.EncodeToString([]byte("a|b|c")),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeCursor(%q) panicked: %v", in, r)
				}
			}()
			_, _, _, _ = DecodeCursor(in)
		}()
	}
}

func TestEncodeDecodeRunCursorRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 5, 12, 34, 56, 789000000, time.UTC)
	const wantID = "a3f1c2d4-5678-4abc-9def-0123456789ab"
	cursor := EncodeRunCursor(want, wantID)

	gotTime, gotID, ok, err := DecodeRunCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeRunCursor(%q): unexpected error: %v", cursor, err)
	}
	if !ok {
		t.Fatalf("DecodeRunCursor(%q): ok = false, want true", cursor)
	}
	if !gotTime.Equal(want) {
		t.Errorf("DecodeRunCursor(%q): time = %v, want %v", cursor, gotTime, want)
	}
	if gotID != wantID {
		t.Errorf("DecodeRunCursor(%q): id = %q, want %q", cursor, gotID, wantID)
	}
}

func TestDecodeRunCursorEmptyMeansNoCursor(t *testing.T) {
	gotTime, gotID, ok, err := DecodeRunCursor("")
	if err != nil {
		t.Fatalf("DecodeRunCursor(\"\"): unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("DecodeRunCursor(\"\"): ok = true, want false")
	}
	if !gotTime.IsZero() || gotID != "" {
		t.Errorf("DecodeRunCursor(\"\"): got (%v, %q), want zero values", gotTime, gotID)
	}
}

func TestDecodeRunCursorMalformedBase64(t *testing.T) {
	_, _, ok, err := DecodeRunCursor("not-valid-base64!!!")
	if err == nil {
		t.Fatal("DecodeRunCursor(malformed base64): expected error, got nil")
	}
	if ok {
		t.Error("DecodeRunCursor(malformed base64): ok = true, want false")
	}
}

func TestDecodeRunCursorMissingSeparator(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("2026-08-05T12:34:56.789Z_no_separator_here"))
	_, _, ok, err := DecodeRunCursor(raw)
	if err == nil {
		t.Fatal("DecodeRunCursor(missing separator): expected error, got nil")
	}
	if ok {
		t.Error("DecodeRunCursor(missing separator): ok = true, want false")
	}
}

func TestDecodeRunCursorBadTimestamp(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("not-a-timestamp|a3f1c2d4-5678-4abc-9def-0123456789ab"))
	_, _, ok, err := DecodeRunCursor(raw)
	if err == nil {
		t.Fatal("DecodeRunCursor(bad timestamp): expected error, got nil")
	}
	if ok {
		t.Error("DecodeRunCursor(bad timestamp): ok = true, want false")
	}
}

func TestDecodeRunCursorNonUUIDID(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("2026-08-05T12:34:56.789Z|not-a-uuid"))
	_, _, ok, err := DecodeRunCursor(raw)
	if err == nil {
		t.Fatal("DecodeRunCursor(non-uuid id): expected error, got nil")
	}
	if ok {
		t.Error("DecodeRunCursor(non-uuid id): ok = true, want false")
	}
}

func TestEncodeRunCursorIsStableAcrossCalls(t *testing.T) {
	ts := time.Date(2026, 8, 5, 12, 34, 56, 789000000, time.UTC)
	const id = "a3f1c2d4-5678-4abc-9def-0123456789ab"
	first := EncodeRunCursor(ts, id)
	second := EncodeRunCursor(ts, id)
	if first != second {
		t.Errorf("EncodeRunCursor is not stable: %q != %q", first, second)
	}
}

func TestDecodeRunCursorNeverPanics(t *testing.T) {
	inputs := []string{
		"",
		"!!!",
		"====",
		base64.RawURLEncoding.EncodeToString([]byte("|")),
		base64.RawURLEncoding.EncodeToString([]byte("||")),
		base64.RawURLEncoding.EncodeToString([]byte("a|b|c")),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeRunCursor(%q) panicked: %v", in, r)
				}
			}()
			_, _, _, _ = DecodeRunCursor(in)
		}()
	}
}
