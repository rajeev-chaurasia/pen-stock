package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// hashScheme names the canonicalization rules a hash was produced under.
// It is part of the hashed preimage, so changing those rules changes
// every hash. That retires entries keyed under the old scheme instead of
// serving them to a request canonicalized under the new one.
const hashScheme = "penstock/cache/exact/v1"

// ErrNotJSONObject reports a body that parses as JSON but is not an
// object. A chat request is always an object, and a body that is not one
// is not a request whose cache safety can be judged.
var ErrNotJSONObject = errors.New("cache: request body is not a JSON object")

// ErrTrailingData reports bytes after the first JSON value. Ignoring
// them would let two different bodies produce one key, and the part
// ignored could be the part that changes the answer.
var ErrTrailingData = errors.New("cache: request body has trailing data after the JSON object")

// unhashedFields are the top level request fields that do not change the
// model's answer. They are dropped before hashing so they cannot cause a
// miss between two requests that are the same question:
//
//   - stream and stream_options choose a transport, not an answer. An
//     Entry holds both the whole body and the streamed frames, so one
//     entry serves a caller who asked for either.
//   - user identifies an end user to the provider for abuse tracking. It
//     is not an input to sampling, and it is not what keeps callers
//     apart here: Tenant is.
//   - metadata is caller side bookkeeping the provider does not sample
//     against.
//
// Everything else is hashed, including fields this gateway does not
// model. That direction is deliberate: an unknown field that does change
// the answer costs a miss, while an unknown field wrongly dropped would
// serve the wrong answer.
//
// Only the top level is pruned. A key of the same name nested inside a
// message or a content part is part of the question and stays hashed.
var unhashedFields = map[string]struct{}{
	"stream":         {},
	"stream_options": {},
	"user":           {},
	"metadata":       {},
}

// BuildKey derives the cache key for a request body sent to a model on
// behalf of a tenant.
//
// The hash covers the request's meaning rather than its bytes. The body
// is decoded and re-encoded canonically, so two requests that differ
// only in JSON key order or in insignificant whitespace hash alike. Two
// clients serializing the same conversation from different libraries ask
// the same question, and hashing the raw bytes would call them
// different.
//
// The digest is SHA-256 rather than a faster non cryptographic hash
// because the hashed content is attacker influenced: a caller chooses
// its own prompt. With a hash that can be collided on demand, one caller
// could craft a request that lands on another request's key and be
// served an answer that was never theirs. The cost of SHA-256 is
// invisible next to the model call it avoids.
//
// Tenant is carried on the Key and never folded into the hash. Isolation
// is enforced by where an entry is stored, not by the digest, and
// keeping the tenant out of the hash lets a test prove that by
// inspecting the key.
//
// The routed model is folded in because it may differ from the model
// named in the body once an alias is resolved, and it is the routed
// model that produces the answer.
func BuildKey(tenant, model string, raw []byte) (Key, error) {
	canonical, err := canonicalize(raw)
	if err != nil {
		return Key{}, err
	}

	// The model is length prefixed so that a model name cannot be
	// mistaken for the beginning of the body, which would let two
	// different requests share a preimage.
	preimage := make([]byte, 0, len(hashScheme)+len(model)+len(canonical)+16)
	preimage = append(preimage, hashScheme...)
	preimage = append(preimage, strconv.Itoa(len(model))...)
	preimage = append(preimage, 0)
	preimage = append(preimage, model...)
	preimage = append(preimage, canonical...)

	sum := sha256.Sum256(preimage)
	return Key{
		Tenant: tenant,
		Model:  model,
		Hash:   hex.EncodeToString(sum[:]),
	}, nil
}

// canonicalize returns one byte form for every spelling of the same
// request: object keys sorted at every level, no insignificant
// whitespace, and fields that do not change the answer removed.
//
// Numbers keep the literal the caller sent rather than being routed
// through a float, so no two distinct values can round together into
// one key. The cost is that 1 and 1.0 hash differently, which is a miss
// and not a wrong answer.
func canonicalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var body any
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("cache: decode request body: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, ErrTrailingData
	}

	fields, ok := body.(map[string]any)
	if !ok {
		return nil, ErrNotJSONObject
	}
	for name := range unhashedFields {
		delete(fields, name)
	}

	// Marshaling a map sorts its keys, at every level of nesting, which
	// is the whole of the canonical ordering rule.
	canonical, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("cache: re-encode request body: %w", err)
	}
	return canonical, nil
}
