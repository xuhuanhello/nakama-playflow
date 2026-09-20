package fleetmanager

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type AdmissionClaims struct {
	SchemaVersion int    `json:"schema_version"`
	UserID        string `json:"user_id"`
	WorkerID      string `json:"worker_id"`
	BootID        string `json:"boot_id"`
	RoomID        string `json:"room_id"`
	AllocationID  string `json:"allocation_id"`
	ReservationID string `json:"reservation_id"`
	Seat          int    `json:"seat"`
	Epoch         int64  `json:"epoch"`
	ExpiresAt     int64  `json:"exp"`
	Nonce         string `json:"nonce"`
	Resume        bool   `json:"resume"`
}

func id() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func derive(key []byte, purpose string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(purpose))
	return h.Sum(nil)
}
func encoded(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }
func sign(key []byte, c AdmissionClaims) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := encoded(b)
	return payload + "." + encoded(derive(key, payload)), nil
}

// VerifyAdmission validates the portable token format. The game host must also
// enforce current room roster, epoch, one-time nonce and active seat ownership.
func VerifyAdmission(key []byte, token string, now int64) (AdmissionClaims, error) {
	var claims AdmissionClaims
	if len(token) > 4096 {
		return claims, fmt.Errorf("invalid admission token")
	}
	p := strings.Split(token, ".")
	if len(p) != 2 {
		return claims, fmt.Errorf("invalid admission token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil || !hmac.Equal(sig, derive(key, p[0])) {
		return claims, fmt.Errorf("invalid admission signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return claims, fmt.Errorf("invalid admission payload")
	}
	if err = json.Unmarshal(raw, &claims); err != nil {
		return claims, fmt.Errorf("invalid admission payload")
	}
	if claims.SchemaVersion != 1 || claims.UserID == "" || claims.WorkerID == "" || claims.BootID == "" || claims.RoomID == "" || claims.AllocationID == "" || claims.ReservationID == "" || claims.Nonce == "" || claims.Epoch < 1 || claims.Seat < 0 || claims.ExpiresAt <= now || claims.ExpiresAt > now+60 {
		return claims, fmt.Errorf("invalid admission claims")
	}
	return claims, nil
}
