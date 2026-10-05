// Package hashring implements versioned, deterministic rendezvous placement.
package hashring

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

const Algorithm = "sha256-hrw-v1"

type Spec struct {
	Epoch   int64    `json:"epoch"`
	Buckets int      `json:"buckets"`
	Shards  []string `json:"shards"`
}
type Ring struct {
	Spec        Spec
	Fingerprint string
	owners      []string
}
type Move struct {
	Bucket int    `json:"bucket"`
	From   string `json:"from"`
	To     string `json:"to"`
}

func New(spec Spec) (*Ring, error) {
	if spec.Epoch < 1 || spec.Buckets < 1 || spec.Buckets > 65536 || spec.Buckets&(spec.Buckets-1) != 0 || len(spec.Shards) < 1 || len(spec.Shards) > 256 {
		return nil, fmt.Errorf("invalid topology bounds")
	}
	spec.Shards = append([]string(nil), spec.Shards...)
	sort.Strings(spec.Shards)
	for i, s := range spec.Shards {
		if s == "" || len(s) > 64 || (i > 0 && s == spec.Shards[i-1]) {
			return nil, fmt.Errorf("invalid or duplicate shard id")
		}
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return nil, fmt.Errorf("shard ids must use a-z, 0-9 or -")
			}
		}
	}
	b, _ := json.Marshal(struct {
		Algorithm string
		Spec      Spec
	}{Algorithm, spec})
	digest := sha256.Sum256(b)
	r := &Ring{Spec: spec, Fingerprint: hex.EncodeToString(digest[:]), owners: make([]string, spec.Buckets)}
	for bucket := range r.owners {
		var best [32]byte
		for i, id := range spec.Shards {
			input := append([]byte("subscriptions/hrw/v1\x00"), make([]byte, 4)...)
			binary.BigEndian.PutUint32(input[len(input)-4:], uint32(bucket))
			input = append(input, []byte(id)...)
			score := sha256.Sum256(input)
			if i == 0 || bytes.Compare(score[:], best[:]) > 0 {
				best = score
				r.owners[bucket] = id
			}
		}
	}
	return r, nil
}

func (r *Ring) Bucket(user uuid.UUID) int {
	const prefix = "subscriptions/user/v1\x00"
	var input [len(prefix) + 16]byte
	copy(input[:], prefix)
	copy(input[len(prefix):], user[:])
	sum := sha256.Sum256(input[:])
	return int(binary.BigEndian.Uint32(sum[:4]) & uint32(r.Spec.Buckets-1))
}
func (r *Ring) Owner(bucket int) string            { return r.owners[bucket] }
func (r *Ring) Route(user uuid.UUID) (int, string) { b := r.Bucket(user); return b, r.Owner(b) }

func Plan(old, next *Ring) ([]Move, error) {
	if next.Spec.Epoch != old.Spec.Epoch+1 || next.Spec.Buckets != old.Spec.Buckets {
		return nil, fmt.Errorf("keep bucket count unchanged and increment epoch by exactly one")
	}
	result := make([]Move, 0)
	for b := 0; b < old.Spec.Buckets; b++ {
		if old.Owner(b) != next.Owner(b) {
			result = append(result, Move{b, old.Owner(b), next.Owner(b)})
		}
	}
	return result, nil
}
