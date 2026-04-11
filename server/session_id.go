package flaresolverr

import (
	"crypto/rand"
	"encoding/hex"
)

func newSessionID() string {
	var payload [16]byte
	if _, err := rand.Read(payload[:]); err != nil {
		return "session"
	}
	return hex.EncodeToString(payload[:])
}
