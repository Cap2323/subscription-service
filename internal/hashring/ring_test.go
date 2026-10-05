package hashring

import (
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
)

func mustRing(t testing.TB, spec Spec) *Ring {
	t.Helper()
	r, e := New(spec)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestDeterministicPermutation(t *testing.T) {
	a := mustRing(t, Spec{1, 1024, []string{"a", "b", "c"}})
	b := mustRing(t, Spec{1, 1024, []string{"c", "a", "b"}})
	if a.Fingerprint != b.Fingerprint {
		t.Fatal("topology depends on input order")
	}
	for bucket := 0; bucket < 1024; bucket++ {
		if a.Owner(bucket) != b.Owner(bucket) {
			t.Fatal(bucket)
		}
	}
	otherEpoch := mustRing(t, Spec{2, 1024, []string{"a", "b", "c"}})
	if a.Fingerprint == otherEpoch.Fingerprint {
		t.Fatal("epoch not fenced")
	}
}
func TestMinimalRemapping(t *testing.T) {
	old := mustRing(t, Spec{1, 1024, []string{"a", "b", "c"}})
	add := mustRing(t, Spec{2, 1024, []string{"a", "b", "c", "d"}})
	moves, e := Plan(old, add)
	if e != nil {
		t.Fatal(e)
	}
	if len(moves) < 180 || len(moves) > 340 {
		t.Fatal("unexpected movement", len(moves))
	}
	for _, move := range moves {
		if move.To != "d" {
			t.Fatal("addition moved a bucket between old nodes", move)
		}
	}
	removed := mustRing(t, Spec{2, 1024, []string{"a", "c"}})
	moves, e = Plan(old, removed)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range moves {
		if m.From != "b" {
			t.Fatal("removal moved an unaffected bucket")
		}
	}
	if _, e := Plan(old, mustRing(t, Spec{2, 2048, []string{"a", "b", "c"}})); e == nil {
		t.Fatal("allowed bucket count change")
	}
}
func TestDistributionMillionUsers(t *testing.T) {
	r := mustRing(t, Spec{1, 1024, []string{"a", "b", "c"}})
	counts := map[string]int{}
	for i := uint64(0); i < 1000000; i++ {
		var id uuid.UUID
		binary.BigEndian.PutUint64(id[8:], i)
		_, owner := r.Route(id)
		counts[owner]++
	}
	for id, n := range counts {
		if n < 280000 || n > 390000 {
			t.Fatalf("skew on %s: %d", id, n)
		}
	}
	t.Logf("1,000,000 deterministic users: %v", counts)
}
func TestRejectInvalidTopology(t *testing.T) {
	for _, s := range []Spec{{0, 1024, []string{"a"}}, {1, 1000, []string{"a"}}, {1, 1024, []string{}}, {1, 1024, []string{"a", "a"}}, {1, 1024, []string{"bad/name"}}} {
		if _, e := New(s); e == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}
func BenchmarkRoute(b *testing.B) {
	r := mustRing(b, Spec{1, 1024, []string{"a", "b", "c", "d", "e", "f", "g", "h"}})
	id := uuid.MustParse("60601fee-2bf1-4721-ae6f-7636e79a0cba")
	b.ReportAllocs()
	for b.Loop() {
		r.Route(id)
	}
}
