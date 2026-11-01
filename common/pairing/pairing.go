// Package pairing wraps CPace (docs/protocol.md §3.2) and the HMACs that bind
// the enrollment messages to the pairing-code session key.
package pairing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"

	"filippo.io/cpace"

	"aicluster/common/proto"
)

const (
	controllerID = "aicluster-controller"
	contextLabel = "aicluster-v1"
	CodeDigits   = 6
)

func contextInfo(nodeID string, nonceS []byte, bootstrapFP string) *cpace.ContextInfo {
	ad := append([]byte(contextLabel), nonceS...)
	ad = append(ad, bootstrapFP...)
	return cpace.NewContextInfo(controllerID, nodeID, ad)
}

// NewCode returns a uniformly random 6-digit code.
func NewCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

// ControllerStart is run by the Controller with the code the admin typed in.
func ControllerStart(code, nodeID string, nonceS []byte, bootstrapFP string) (msgA []byte, st *cpace.State, err error) {
	return cpace.Start(code, contextInfo(nodeID, nonceS, bootstrapFP))
}

// SlaveExchange is run by the Slave with the code shown on its console.
func SlaveExchange(code, nodeID string, nonceS []byte, bootstrapFP string, msgA []byte) (msgB, key []byte, err error) {
	return cpace.Exchange(code, contextInfo(nodeID, nonceS, bootstrapFP), msgA)
}

// MAC computes HMAC-SHA256(key, label || len-prefixed parts...). A nil key
// (no pairing) returns nil so callers can leave the field out.
func MAC(key []byte, label string, parts ...[]byte) []byte {
	if key == nil {
		return nil
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	var l [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(l[:], uint32(len(p)))
		m.Write(l[:])
		m.Write(p)
	}
	return m.Sum(nil)
}

// CheckMAC verifies a MAC in constant time. With a nil key it accepts only an
// empty MAC.
func CheckMAC(key []byte, got []byte, label string, parts ...[]byte) bool {
	if key == nil {
		return len(got) == 0
	}
	return hmac.Equal(got, MAC(key, label, parts...))
}

func Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// CompleteMACInput is what the "complete" MAC covers (§3.4): the hash of the
// JSON body with MAC cleared. Both sides marshal the same Go struct, so the
// encoding is identical.
func CompleteMACInput(c proto.EnrollComplete) []byte {
	c.MAC = nil
	b, _ := json.Marshal(c)
	return Sum(b)
}
