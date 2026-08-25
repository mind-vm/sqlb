// Package vault is the write path a generated create body cannot carry.
package vault

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/mind-vm/sqlb"
)

// devKey is a fixed, published key. This is not a cipher and must never be
// treated as one — it exists so the example has something to XOR, so the
// point being made (a generated create body cannot name a Hidden column, so
// the write goes through Go instead) does not get lost inside a real crypto
// library's API. A real vault calls out to a real KMS; see the README for
// what this deliberately does not attempt.
var devKey = []byte("not-a-real-key-do-not-reuse-me!")

func xor(data, key []byte) []byte {
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ key[i%len(key)]
	}
	return out
}

// Encrypt is the generated create body's replacement: the only path that
// writes ciphertext, nonce and key_version, because Secret's create body has
// nothing to name them with (rest_gen.go mounts rest.None[Secret] on both
// sides). It calls sqlb.InsertRows directly, the same way example/blog's own
// tests set password_hash — Hidden changes what a generated surface can
// reach, not what Go code can do.
func Encrypt(ctx context.Context, db sqlb.Executor, ownerKind, ownerID string, plaintext []byte) (*Secret, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: nonce: %w", err)
	}
	s := &Secret{
		OwnerKind:  ownerKind,
		OwnerID:    ownerID,
		Ciphertext: xor(plaintext, devKey),
		Nonce:      nonce,
		KeyVersion: 1,
	}
	got, err := sqlb.InsertRows(s).One(ctx, db)
	if err != nil {
		return nil, err
	}
	return &got, nil
}

// Decrypt reverses Encrypt. It reads at the Go/library level — Hidden blocks
// a REST response and the generated typed-column facade, not a plain
// sqlb.Query[Secret] a Go caller in this process makes directly.
func Decrypt(s *Secret) []byte {
	return xor(s.Ciphertext, devKey)
}
