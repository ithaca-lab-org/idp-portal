package session

import (
	"net/http"
	"net/http/httptest"
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
	v, err := c.Encode("p", map[string]string{"sub": "u1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := c.Decode("p", v, &got); err != nil || got["sub"] != "u1" {
		t.Fatalf("decode: %v %v", got, err)
	}
}

func TestPurposeBinding(t *testing.T) {
	c := testCodec(t)
	v, _ := c.Encode("idp_session", map[string]string{"sub": "u1"}, time.Minute)
	var got map[string]string
	if err := c.Decode("idp_login", v, &got); err != ErrInvalid {
		t.Fatalf("cookie minted for one purpose must not decode as another, got %v", err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	c := testCodec(t)
	a, _ := c.Encode("p", map[string]string{"sub": "u1"}, time.Minute)
	b, _ := c.Encode("p", map[string]string{"sub": "admin"}, time.Minute)
	forged := strings.SplitN(b, ".", 2)[0] + "." + strings.SplitN(a, ".", 2)[1]
	var got map[string]string
	if err := c.Decode("p", forged, &got); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	c := testCodec(t)
	other, _ := NewCodec([]byte(strings.Repeat("x", 32)), true)
	v, _ := c.Encode("p", "hi", time.Minute)
	var s string
	if err := other.Decode("p", v, &s); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestExpiredRejected(t *testing.T) {
	c := testCodec(t)
	v, _ := c.Encode("p", "hi", time.Minute)
	c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	var s string
	if err := c.Decode("p", v, &s); err != ErrInvalid {
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
		if err := c.Decode("p", v, &s); err != ErrInvalid {
			t.Fatalf("%q: want ErrInvalid, got %v", v, err)
		}
	}
}

func TestOversizeRejected(t *testing.T) {
	c := testCodec(t)
	if _, err := c.Encode("p", strings.Repeat("g", 5000), time.Minute); err != ErrTooLarge {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if err := c.Set(httptest.NewRecorder(), "p", strings.Repeat("g", 5000), time.Minute); err != ErrTooLarge {
		t.Fatalf("Set: want ErrTooLarge, got %v", err)
	}
}

func TestNameUsesHostPrefixOnlyWhenSecure(t *testing.T) {
	if got := testCodec(t).Name("idp_session"); got != "__Host-idp_session" {
		t.Fatalf("secure name %q", got)
	}
	c, _ := NewCodec([]byte(strings.Repeat("k", 32)), false)
	if got := c.Name("idp_session"); got != "idp_session" {
		t.Fatalf("insecure name %q", got)
	}
}

func TestSetCookieAttributes(t *testing.T) {
	c := testCodec(t)
	rec := httptest.NewRecorder()
	if err := c.Set(rec, c.Name("idp_session"), "x", time.Minute); err != nil {
		t.Fatal(err)
	}
	ck := rec.Result().Cookies()[0]
	if !ck.HttpOnly || !ck.Secure || ck.Path != "/" || ck.Domain != "" || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("__Host- cookie needs Secure, Path=/, no Domain: %+v", ck)
	}
}
