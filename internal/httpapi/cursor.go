package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Cursor struct {
	User uuid.UUID `json:"u"`
	Name string    `json:"n"`
	Time time.Time `json:"t"`
	ID   uuid.UUID `json:"i"`
	Exp  int64     `json:"e"`
}
type CursorCodec struct{ Key []byte }

func (c CursorCodec) Encode(v Cursor) string {
	b, _ := json.Marshal(v)
	mac := hmac.New(sha256.New, c.Key)
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (c CursorCodec) Decode(raw string, user uuid.UUID, name string) (Cursor, error) {
	var v Cursor
	if len(raw) > 2048 {
		return v, fmt.Errorf("invalid cursor")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return v, fmt.Errorf("invalid cursor")
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return v, fmt.Errorf("invalid cursor")
	}
	signature, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return v, fmt.Errorf("invalid cursor")
	}
	mac := hmac.New(sha256.New, c.Key)
	mac.Write(b)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return v, fmt.Errorf("invalid cursor")
	}
	if e := json.Unmarshal(b, &v); e != nil || v.User != user || v.Name != name || v.Time.IsZero() || v.ID == uuid.Nil || v.Exp <= time.Now().Unix() {
		return v, fmt.Errorf("expired cursor or different user/filter")
	}
	return v, nil
}
