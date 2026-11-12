// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// contentHash is the digest a lockfile records of its own content: the
// SHA-256, in SRI form, of the lockfile without its contentHash field, in
// the form Nix's builtins.toJSON gives the same value: no whitespace,
// object keys in sorted order, and no escaping beyond what JSON requires.
// lib-overlays/default/lock.nix computes the same digest.
func contentHash(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	delete(value, "contentHash")
	var canonical bytes.Buffer
	enc := json.NewEncoder(&canonical)
	enc.SetEscapeHTML(false)
	// encoding/json writes the keys of a map in sorted order.
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	return sri(sum[:]), nil
}
