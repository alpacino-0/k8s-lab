/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

// Package auth turns a password or a cookie into a subject the authorizer can
// answer about.
//
// It costs no new dependencies: golang.org/x/crypto is already in the module
// graph, and everything else here is the standard library.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrMismatch is returned when a password does not match its hash. It is the
// only failure a caller should distinguish, and callers must not distinguish it
// from an unknown account — see VerifyDummy.
var ErrMismatch = errors.New("auth: password does not match")

// Params are the argon2id cost parameters.
//
// They are stored inside every hash rather than held in configuration, so they
// can be raised later without a flag day: an old hash still verifies with the
// parameters it was made under, and is rewritten at the next successful login.
type Params struct {
	// Memory in KiB.
	Memory uint32
	// Time is the number of passes.
	Time uint32
	// Parallelism is the number of lanes.
	Parallelism uint8
	// SaltLength and KeyLength in bytes.
	SaltLength uint32
	KeyLength  uint32
}

// DefaultParams is OWASP's second recommendation rather than its first, and the
// difference is the whole point.
//
// The first is 46 MiB with one pass; this is 19 MiB with two, which OWASP holds
// equivalent. Nineteen is chosen because this runs in a container with a memory
// limit and argon2id's cost is memory that is actually allocated: four
// concurrent logins at 46 MiB is 184 MiB of live heap, which OOMKills a control
// plane sized for anything reasonable — and an OOMKill during a login storm
// takes the panel down for everyone, not just the person logging in.
//
// Parallelism is 1 for the same reason inverted: lanes buy speed only when
// there are cores to spare, and the pod this runs in is CPU-limited. Asking for
// four lanes under a fractional CPU limit spends the scheduler's patience
// without finishing sooner.
var DefaultParams = Params{
	Memory:      19 * 1024,
	Time:        2,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

// Hasher hashes and verifies passwords, with a bound on how many it will do at
// once.
//
// The bound is the part that matters. Any memory parameter, however carefully
// chosen, multiplies by the number of concurrent hashes — so a login page
// without one is a memory limit that holds until somebody points a script at
// it. Tuning the parameter down instead would weaken every password to survive
// a burst; bounding concurrency makes the peak predictable and costs a waiting
// request some latency, which is the right thing to spend.
type Hasher struct {
	params Params
	gate   chan struct{}
}

// NewHasher returns a Hasher. concurrency of zero picks a bound from the CPUs
// visible to the process, which under a Kubernetes limit is the node's count
// rather than the pod's — so it is deliberately conservative.
func NewHasher(p Params, concurrency int) *Hasher {
	if p.Memory == 0 {
		p = DefaultParams
	}
	if concurrency <= 0 {
		concurrency = max(2, runtime.GOMAXPROCS(0)/2)
	}
	return &Hasher{params: p, gate: make(chan struct{}, concurrency)}
}

// Hash returns an encoded hash, parameters included.
func (h *Hasher) Hash(password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: empty password")
	}
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: reading salt: %w", err)
	}
	key := h.derive(password, salt, h.params)
	return encode(h.params, salt, key), nil
}

// Verify reports whether password matches encoded. It returns ErrMismatch for a
// wrong password and a different error for a hash it cannot read — a corrupt
// row is not a failed login, and reporting it as one would hide a data problem
// behind a user mistake.
func (h *Hasher) Verify(encoded, password string) error {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return err
	}
	got := h.derive(password, salt, p)
	// Constant time, because the comparison is over a secret-derived value and
	// an early exit leaks how much of it matched.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was made with weaker parameters
// than the ones in force. A login is the only moment the plaintext exists, so
// it is the only moment a hash can be upgraded.
func (h *Hasher) NeedsRehash(encoded string) bool {
	p, _, _, err := decode(encoded)
	if err != nil {
		return true
	}
	return p.Memory < h.params.Memory || p.Time < h.params.Time
}

// VerifyDummy burns the same work as a real verification.
//
// A login handler that skips hashing when the address is unknown answers faster
// for addresses that do not exist, and that difference is an account
// enumeration oracle — measurable over the network without any error message
// giving it away. So an unknown address takes this path and the handler says
// the same thing either way.
func (h *Hasher) VerifyDummy(password string) {
	salt := make([]byte, h.params.SaltLength)
	_ = h.derive(password, salt, h.params)
}

func (h *Hasher) derive(password string, salt []byte, p Params) []byte {
	// Held for the duration of the derivation, which is what makes the peak
	// memory bounded rather than merely typical.
	h.gate <- struct{}{}
	defer func() { <-h.gate }()
	return argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Parallelism, p.KeyLength)
}

// encode writes the PHC string format, which is what lets the parameters travel
// with the hash instead of living in a config file the hash cannot see.
func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("auth: unrecognised hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("auth: unreadable hash version: %w", err)
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("auth: hash version %d, this build understands %d",
			version, argon2.Version)
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Parallelism); err != nil {
		return Params{}, nil, nil, fmt.Errorf("auth: unreadable hash parameters: %w", err)
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("auth: unreadable salt: %w", err)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("auth: unreadable hash: %w", err)
	}
	p.SaltLength, p.KeyLength = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}
