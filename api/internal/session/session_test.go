package session

import (
	"strings"
	"testing"
	"time"
)

func testCodec(t *testing.T) *Codec {
	t.Helper()
	c, err := NewCodec([]byte(strings.Repeat("k", 32)), true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := testCodec(t)
	v, err := c.Encode(map[string]string{"sub": "u1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := c.Decode(v, &got); err != nil || got["sub"] != "u1" {
		t.Fatalf("decode: %v %v", got, err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	c := testCodec(t)
	a, _ := c.Encode(map[string]string{"sub": "u1"}, time.Minute)
	b, _ := c.Encode(map[string]string{"sub": "admin"}, time.Minute)
	forged := strings.SplitN(b, ".", 2)[0] + "." + strings.SplitN(a, ".", 2)[1]
	var got map[string]string
	if err := c.Decode(forged, &got); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	c := testCodec(t)
	other, _ := NewCodec([]byte(strings.Repeat("x", 32)), true)
	v, _ := c.Encode("hi", time.Minute)
	var s string
	if err := other.Decode(v, &s); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestExpiredRejected(t *testing.T) {
	c := testCodec(t)
	v, _ := c.Encode("hi", time.Minute)
	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	var s string
	if err := c.Decode(v, &s); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := NewCodec([]byte("short"), true); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestGarbageRejected(t *testing.T) {
	c := testCodec(t)
	for _, v := range []string{"", "nodot", "a.b", "!!!.???"} {
		var s string
		if err := c.Decode(v, &s); err != ErrInvalid {
			t.Fatalf("%q: want ErrInvalid, got %v", v, err)
		}
	}
}
