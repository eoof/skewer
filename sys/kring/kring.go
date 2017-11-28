package kring

import (
	"crypto/rand"
	"fmt"
	"io"

	"github.com/awnumar/memguard"
	"github.com/oklog/ulid"
	"github.com/stephane-martin/skewer/sys/semaphore"
	"github.com/stephane-martin/skewer/utils"
)

type Ring interface {
	NewSignaturePubkey() (privkey *memguard.LockedBuffer, err error)
	GetSignaturePubkey() (pubkey *memguard.LockedBuffer, err error)
	NewBoxSecret() (secret *memguard.LockedBuffer, err error)
	GetBoxSecret() (secret *memguard.LockedBuffer, err error)
	DeleteBoxSecret() error
	DeleteSignaturePubKey() error
	WriteRingPass(io.Writer) error
	GetSessionID() ulid.ULID
	Destroy()
}

func NewSecret() (m *memguard.LockedBuffer, err error) {
	secretKey := make([]byte, 32)
	_, err = rand.Read(secretKey)
	if err != nil {
		return nil, err
	}
	m, err = memguard.NewImmutableFromBytes(secretKey)
	if err != nil {
		return nil, err
	}
	return m, nil
}

type RingCreds struct {
	SessionID ulid.ULID
	Secret    *memguard.LockedBuffer
}

func NewCreds() (creds RingCreds, err error) {
	var secret *memguard.LockedBuffer
	secret, err = NewSecret()
	if err != nil {
		return
	}
	creds.SessionID = utils.NewUid()
	creds.Secret = secret
	return creds, nil
}

func destroySem(sessionID ulid.ULID) {
	semaphore.Destroy(fmt.Sprintf("skw%s", sessionID.String()))
}