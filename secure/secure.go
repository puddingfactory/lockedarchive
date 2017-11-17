package secure

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"unsafe"

	"github.com/awnumar/memguard"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

// Container is responsible for securing encryption keys and passphrases in memory
type container struct {
	*memguard.LockedBuffer
}

// KeyContainer is responsible for securing encryption keys in memory
type KeyContainer container

// PassphraseContainer is responsible for securing passphrases in memory
type PassphraseContainer container

// Nonce is used in encryption and should be random, but not secret
type Nonce = *[NonceSize]byte

// Key is used in encryption and should be kept secret
type Key = *[KeySize]byte

// Salt is used in derriving a key from a passphrase
type Salt = *[SaltSize]byte

const (

	// KeySize represents the size of the key in bytes
	KeySize = 32 // 256-bit

	// NonceSize represents the size of the nonce in bytes
	NonceSize = 24 // 192-bit

	// SaltSize represents the size of the salt in bytes
	SaltSize = 8 // 64-bit
)

var (

	// ErrEncrypt is an error that occurred during encryption
	ErrEncrypt = errors.New("secret: encryption failed")

	// ErrDecrypt is an error that occurred during decryption
	ErrDecrypt = errors.New("secret: decryption failed")

	// ErrPassphraseContainerNotSet is an error that occurred during an Encryption/Decryption operation with a passphrase
	ErrPassphraseContainerNotSet = errors.New("passphrase not set")
)

// Key returns an unsafe pointer to a byte array for use in encryption/decryption methods
func (kc *KeyContainer) Key() Key {
	return (Key)(unsafe.Pointer(&kc.Buffer()[0]))
}

// REVIEW: https://leanpub.com/gocrypto/read#leanpub-auto-nacl

// GenerateKeyContainer creates a new random secret key inside a safe container
func GenerateKeyContainer() (*KeyContainer, error) {
	buf, err := memguard.NewImmutableRandom(KeySize)
	return &KeyContainer{LockedBuffer: buf}, err
}

// ProtectPassphrase copies passphrase bytes to a safe place in memory and wipes the original
func ProtectPassphrase(passphrase []byte) (*PassphraseContainer, error) {
	buf, err := memguard.NewImmutableFromBytes(passphrase)
	return &PassphraseContainer{LockedBuffer: buf}, err
}

// DeriveKeyContainer generates a new KeyContainer from a passphrase and wipes passphrase's bytes once done, even on err
func (pc *PassphraseContainer) DeriveKeyContainer(salt Salt) (*KeyContainer, error) {

	// Make a copy of passphrase to pass to scrypt, unfortunately.
	// Passing pc.LockedBuffer.Buffer() directly was problematic
	// since the underlying byte slice would unexpectedly wipe during
	// scrypt's calls to hmac.New.
	pass := make([]byte, pc.LockedBuffer.Size())
	copy(pass, pc.LockedBuffer.Buffer())
	if bytes.Equal(pass, make([]byte, pc.LockedBuffer.Size())) {
		// TODO: this check really shouldn't be necessary in the long-term, but
		// I'm monitoring it for now because of the strange behavior from before.
		return nil, errors.New("tried to copy from passphrase container, but it was wiped already")
	}

	keyBytes, err := scrypt.Key(pass, salt[:], 1<<15, 8, 1, KeySize)
	Wipe(pass)
	if err != nil {
		return nil, err
	}

	// NOTE: keyBytes are automatically wiped during this call
	buf, err := memguard.NewImmutableFromBytes(keyBytes)
	kc := &KeyContainer{LockedBuffer: buf}

	return kc, err
}

// GenerateSalt creates a new random Salt
func GenerateSalt() (Salt, error) {
	salt := new([SaltSize]byte)
	_, err := io.ReadFull(rand.Reader, salt[:])
	return salt, err
}

// GenerateNonce creates a new random Nonce
func GenerateNonce() (Nonce, error) {
	nonce := new([NonceSize]byte)
	_, err := io.ReadFull(rand.Reader, nonce[:])
	return nonce, err
}

// IncrementNonce treats the received Nonce as big-endian value and increments it
func IncrementNonce(nonce Nonce) {
	for i := NonceSize - 1; i > 0; i-- {
		nonce[i]++
		if nonce[i] != 0 {
			break
		}
	}
}

// EncryptAndWipe encrypts the input using NaCl's secretbox package and the nonce is prepended to the ciphertext.
// A sealed message will the same size as the original message plus secretbox.Overhead bytes long.
// The slice is wiped once the bytes have been encrypted.
func EncryptAndWipe(key Key, nonce Nonce, message []byte) []byte {
	// fmt.Printf("encrypting chunk of size %d\n", len(message)) // TODO: remove (testing)

	out := make([]byte, len(nonce))
	copy(out, nonce[:])
	out = secretbox.Seal(out, message, nonce, key)
	Wipe(message) // zero bytes of original message in memory asap
	return out
}

// Decrypt extracts the nonce from the ciphertext, and attempts to
// decrypt with secretbox.
func Decrypt(key Key, message []byte) ([]byte, error) {
	// fmt.Printf("decrypting chunk of size %d\n", len(message)) // TODO: remove (testing)

	if len(message) < (NonceSize + secretbox.Overhead) {
		return nil, ErrDecrypt
	}

	nonce := new([NonceSize]byte)
	copy(nonce[:], message[:NonceSize])
	out, ok := secretbox.Open(nil, message[NonceSize:], nonce, key)
	if !ok {
		return nil, ErrDecrypt
	}

	return out, nil
}

// EncryptWithSaltAndWipe encrypts the bytes with a key, nonce, and salt.
// The slice is wiped once the bytes have been encrypted.
func EncryptWithSaltAndWipe(pc *PassphraseContainer, nonce Nonce, message []byte) ([]byte, error) {
	if pc == nil {
		return nil, ErrPassphraseContainerNotSet
	}

	salt, err := GenerateSalt()
	if err != nil {
		return nil, err
	}

	kc, err := pc.DeriveKeyContainer(salt)
	if err != nil {
		return nil, err
	}

	encryptedData := EncryptAndWipe(kc.Key(), nonce, message)

	contents := append(salt[:], encryptedData...)
	return contents, nil
}

// DecryptWithSalt extracts the salt and nonce from the ciphertext and attempts to decrypt with secretbox.
func DecryptWithSalt(pc *PassphraseContainer, message []byte) ([]byte, error) {
	if pc == nil {
		return nil, ErrPassphraseContainerNotSet
	}

	salt := new([SaltSize]byte)
	copy(salt[:], message[:SaltSize])
	kc, err := pc.DeriveKeyContainer(salt)
	if err != nil {
		return nil, err
	}

	return Decrypt(kc.Key(), message[SaltSize:])
}

// Wipe attempts to zero out bytes
func Wipe(data []byte) {
	for i := 0; i < len(data); i++ {
		data[i] = 0
	}
}