package bls12381

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	blst "github.com/supranational/blst/bindings/go"

	"github.com/cosmos/cosmos-sdk/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
)

const (
	// PrivKeySize defines the length of the PrivKey byte array.
	PrivKeySize = 32
	// PubKeySize defines the length of the PubKey byte array.
	PubKeySize = 96
	// SignatureLength defines the byte length of a BLS signature.
	SignatureLength = 96
	// KeyType is the string constant for the BLS12-381 algorithm.
	KeyType = "bls12_381"
	// BLS12-381 private key name.
	PrivKeyName = "tendermint/PrivkeyBls12381"
	// BLS12-381 public key name.
	PubKeyName = "tendermint/PubkeyBls12381"
	// Enabled indicates if this curve is enabled.
	Enabled = true
)

var (
	// ErrDeserialization is returned when deserialization fails.
	ErrDeserialization = errors.New("bls12381: deserialization error")
	// ErrInfinitePubKey is returned when the public key is infinite. It is part
	// of a more comprehensive subgroup check on the key.
	ErrInfinitePubKey = errors.New("bls12381: pubkey is infinite")

	dstMinPk = []byte("BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_")
)

// -------------------------------------.

// ===============================================================================================
// Private Key
// ===============================================================================================

// PrivKey is a wrapper around the Ethereum BLS12-381 private key type. This
// wrapper conforms to crypto.Pubkey to allow for the use of the Ethereum
// BLS12-381 private key type.

// var _ crypto.PrivKey = &PrivKey{}
var (
	_ cryptotypes.PrivKey  = &PrivKey{}
	_ codec.AminoMarshaler = &PrivKey{}
)

// GenPrivKeyFromSecret generates a new random key using `secret` for the seed
func GenPrivKeyFromSecret(secret []byte) (*PrivKey, error) {
	if len(secret) != 32 {
		seed := sha256.Sum256(secret) // We need 32 bytes
		secret = seed[:]
	}

	sk := blst.KeyGen(secret)
	return &PrivKey{Key: sk.Serialize()}, nil
}

// NewPrivateKeyFromBytes build a new key from the given bytes.
func NewPrivateKeyFromBytes(bz []byte) (*PrivKey, error) {
	sk := new(blst.SecretKey).Deserialize(bz)
	if sk == nil {
		return nil, ErrDeserialization
	}
	return &PrivKey{Key: sk.Serialize()}, nil
}

// GenPrivKey generates a new key.
func GenPrivKey() (*PrivKey, error) {
	var ikm [32]byte
	_, err := rand.Read(ikm[:])
	if err != nil {
		return nil, err
	}
	return GenPrivKeyFromSecret(ikm[:])
}

// Bytes returns the byte representation of the Key.
func (privKey PrivKey) Bytes() []byte {
	return privKey.Key
}

// PubKey returns ECDSA public key associated with this private key.
func (sk *PrivKey) PubKey() cryptotypes.PubKey {

	return &PubKey{sk.PubKey().Bytes()}
}

// Type returns the type.
func (PrivKey) Type() string {
	return KeyType
}

// Sign signs the given byte array.
func (privKey PrivKey) Sign(msg []byte) ([]byte, error) {
	sk := new(blst.SecretKey).Deserialize(privKey.Bytes())
	signature := new(blstSignature).Sign(sk, msg, dstMinPk)
	return signature.Compress(), nil
}

// Zeroize clears the private key.
func (privKey *PrivKey) Zeroize() {
	sk := new(blst.SecretKey).Deserialize(privKey.Bytes())
	sk.Zeroize()
}

// MarshalJSON marshals the private key to JSON.
//
// XXX: Not a pointer because our JSON encoder (libs/json) does not correctly
// handle pointers.
func (privKey PrivKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(privKey.Bytes())
}

// UnmarshalJSON unmarshals the private key from JSON.
func (privKey *PrivKey) UnmarshalJSON(bz []byte) error {
	var rawBytes []byte
	if err := json.Unmarshal(bz, &rawBytes); err != nil {
		return err
	}
	pk, err := NewPrivateKeyFromBytes(rawBytes)
	if err != nil {
		return err
	}
	privKey.Key = pk.Key
	return nil
}

// Equals returns true if the two private keys are equal.
func (privKey PrivKey) Equals(other cryptotypes.LedgerPrivKey) bool {
	otherBLS, ok := other.(*PrivKey)
	if !ok {
		return false
	}
	return bytes.Equal(privKey.Bytes(), otherBLS.Bytes())
}

// ===============================================================================================
// Public Key
// ===============================================================================================

// Pubkey is a wrapper around the Ethereum BLS12-381 public key type. This
// wrapper conforms to crypto.Pubkey to allow for the use of the Ethereum
// BLS12-381 public key type.

// var _ crypto.PubKey = &PubKey{}
var _ cryptotypes.PubKey = &PubKey{}

// NewPublicKeyFromBytes returns a new public key from the given bytes.
func NewPublicKeyFromBytes(bz []byte) (*PubKey, error) {
	pk := new(blstPublicKey).Deserialize(bz)
	if pk == nil {
		return nil, ErrDeserialization
	}
	// Subgroup and infinity check
	if !pk.KeyValidate() {
		return nil, ErrInfinitePubKey
	}
	return &PubKey{Key: pk.Compress()}, nil
}

// Address returns the address of the key.
// The function will panic if the public key is invalid.
func (pubKey PubKey) Address() cryptotypes.Address {
	return cryptotypes.Address(pubKey.Key)
	// return cryptotypes.Address(tmhash.SumTruncated(pubKey.Key))

}

// VerifySignature verifies the given signature.
func (pubKey PubKey) VerifySignature(msg, sig []byte) bool {
	signature := new(blstSignature).Uncompress(sig)

	if signature == nil {
		return false
	}
	pk := new(blstPublicKey).Deserialize(pubKey.Bytes())
	// Group check signature. Do not check for infinity since an aggregated signature
	// could be infinite.
	if !signature.SigValidate(false) {
		return false
	}

	return signature.Verify(false, pk, false, msg, dstMinPk)
}

// Bytes returns the byte format.
func (pubKey PubKey) Bytes() []byte {
	return pubKey.Key
}

// Type returns the key's type.
func (PubKey) Type() string {
	return KeyType
}

// XXX: Not a pointer because our JSON encoder (libs/json) does not correctly
// handle pointers.
func (pubkey PubKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(pubkey.Bytes())
}

// UnmarshalJSON unmarshals the public key from JSON.
func (pubkey *PubKey) UnmarshalJSON(bz []byte) error {
	var rawBytes []byte
	if err := json.Unmarshal(bz, &rawBytes); err != nil {
		return err
	}
	pk, err := NewPublicKeyFromBytes(rawBytes)
	if err != nil {
		return err
	}
	pubkey.Key = pk.Key
	return nil
}

// Equals returns true if the two public keys are equal.
func (pubKey PubKey) Equals(other cryptotypes.PubKey) bool {
	otherBLS, ok := other.(*PubKey)
	if !ok {
		return false
	}
	return bytes.Equal(pubKey.Bytes(), otherBLS.Bytes())
}

// String implements proto.Message interface.
func (pubKey PubKey) String() string {
	return pubKey.Address().String()
}

// MarshalAmino overrides Amino binary marshaling.
func (privKey PrivKey) MarshalAmino() ([]byte, error) {
	return privKey.Bytes(), nil
}

// UnmarshalAmino overrides Amino binary marshaling.
func (privKey *PrivKey) UnmarshalAmino(bz []byte) error {
	if len(bz) != PrivKeySize {
		return fmt.Errorf("invalid privkey size")
	}

	// Deserialize the secret key from the byte slice
	secretKey := new(blst.SecretKey)
	secretKey.Deserialize(bz)
	if !secretKey.Valid() {
		return fmt.Errorf("secret key invalid")
	}

	privKey.Key = secretKey.Serialize()

	return nil
}

// MarshalAminoJSON overrides Amino JSON marshaling.
func (privKey PrivKey) MarshalAminoJSON() ([]byte, error) {
	// When we marshal to Amino JSON, we don't marshal the "key" field itself,
	// just its contents (i.e. the key bytes).
	return privKey.MarshalAmino()
}

// UnmarshalAminoJSON overrides Amino JSON marshaling.
func (privKey *PrivKey) UnmarshalAminoJSON(bz []byte) error {
	return privKey.UnmarshalAmino(bz)
}

// Compare compares two PubKey instances.
// func (pk *PubKey) Compare(other *PubKey) int {
// 	if other == nil {
// 		if pk == nil {
// 			return 0
// 		}
// 		return 1
// 	}

// 	if pk == nil {
// 		return -1
// 	}

// 	return bytes.Compare(pk.Key.Compress(), other.Key.Compress())
// }

// Compare compares two PubKey instances.
func (pk *PubKey) Equal(other *PubKey) bool {

	return bytes.Equal(pk.GetKey(), other.GetKey())
}
