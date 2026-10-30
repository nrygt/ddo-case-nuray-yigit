package httpx

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cursor keyset sayfalama imleci: (zaman, id) çiftini taşır.
type Cursor struct {
	At time.Time
	ID int64
}

func (c Cursor) Encode() string {
	raw := strconv.FormatInt(c.At.UnixMicro(), 10) + ":" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, Invalid("cursor", "geçersiz imleç")
	}
	at, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return Cursor{}, Invalid("cursor", "geçersiz imleç")
	}
	us, err1 := strconv.ParseInt(at, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil {
		return Cursor{}, Invalid("cursor", "geçersiz imleç")
	}
	return Cursor{At: time.UnixMicro(us).UTC(), ID: n}, nil
}

// PageParams ?limit=&cursor= okur. limit 1..max arası.
func PageParams(r *http.Request, def, max int) (int, *Cursor, error) {
	limit := def
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > max {
			return 0, nil, Invalid("limit", "1 ile "+strconv.Itoa(max)+" arasında olmalı")
		}
		limit = n
	}
	if s := r.URL.Query().Get("cursor"); s != "" {
		c, err := DecodeCursor(s)
		if err != nil {
			return 0, nil, err
		}
		return limit, &c, nil
	}
	return limit, nil, nil
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
