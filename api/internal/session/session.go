// Package session implements stateless signed cookies (HMAC-SHA256 over a JSON payload).
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid or expired cookie")

type Codec struct {
	key    []byte
	Secure bool
	now    func() time.Time
}

func NewCodec(key []byte, secure bool) (*Codec, error) {
	if len(key) < 32 {
		return nil, errors.New("session key must be at least 32 bytes")
	}
	return &Codec{key: key, Secure: secure, now: time.Now}, nil
}

type envelope struct {
	Exp  int64           `json:"exp"`
	Data json.RawMessage `json:"data"`
}

func (c *Codec) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write(payload)
	return m.Sum(nil)
}

// Encode returns "payload.signature" (both base64url) for v, valid for ttl.
func (c *Codec) Encode(v any, ttl time.Duration) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(envelope{Exp: c.now().Add(ttl).Unix(), Data: data})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(c.sign(payload)), nil
}

// Decode verifies the signature and expiry, then unmarshals the data into v.
func (c *Codec) Decode(value string, v any) error {
	p, s, ok := strings.Cut(value, ".")
	if !ok {
		return ErrInvalid
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(p)
	if err != nil {
		return ErrInvalid
	}
	sig, err := enc.DecodeString(s)
	if err != nil || !hmac.Equal(sig, c.sign(payload)) {
		return ErrInvalid
	}
	var e envelope
	if err := json.Unmarshal(payload, &e); err != nil || c.now().Unix() >= e.Exp {
		return ErrInvalid
	}
	return json.Unmarshal(e.Data, v)
}

// Set writes the cookie: HttpOnly, SameSite=Lax, Secure unless disabled for local dev.
func (c *Codec) Set(w http.ResponseWriter, name string, v any, ttl time.Duration) error {
	val, err := c.Encode(v, ttl)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: val, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (c *Codec) Clear(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// Get reads and verifies the named cookie from the request.
func (c *Codec) Get(r *http.Request, name string, v any) error {
	ck, err := r.Cookie(name)
	if err != nil {
		return ErrInvalid
	}
	return c.Decode(ck.Value, v)
}
