package domain

import (
	"encoding/json"
	"testing"
)

func month(t *testing.T, s string) Month {
	t.Helper()
	v, e := ParseMonth(s)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestMonthParsing(t *testing.T) {
	for _, s := range []string{"01-0001", "02-2024", "12-9999"} {
		m, e := ParseMonth(s)
		if e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(m)
		if e != nil || string(b) != `"`+s+`"` {
			t.Fatal(s, string(b), e)
		}
		if FromTime(m.Time()) != m {
			t.Fatal("SQL date round trip")
		}
	}
	for _, s := range []string{"1-2025", "13-2025", "00-2025", "01-0000", "2025-01", "07-2025x", ""} {
		if _, e := ParseMonth(s); e == nil {
			t.Fatal("accepted", s)
		}
	}
}
func TestCreateAndMergedPatch(t *testing.T) {
	start, end := month(t, "07-2025"), month(t, "09-2025")
	valid := Create{ServiceName: " Test ", Price: 400, StartDate: start, EndDate: &end}
	if e := valid.Validate(); e != nil {
		t.Fatal(e)
	}
	if valid.ServiceName != "Test" {
		t.Fatal("not trimmed")
	}
	for _, req := range []Create{{" ", 1, start, nil}, {"a\x00b", 1, start, nil}, {"A", 0, start, nil}, {"A", 2147483648, start, nil}, {"A", 1, 0, nil}, {"A", 1, end, &start}} {
		if e := req.Validate(); e == nil {
			t.Fatal("invalid create accepted", req)
		}
	}
	s := Subscription{ServiceName: "A", Price: 400, StartDate: start, EndDate: &end}
	later := month(t, "10-2025")
	if e := (Patch{StartDate: &later}).Apply(&s); e == nil {
		t.Fatal("merged interval accepted")
	}
	if s.StartDate != start {
		t.Fatal("failed patch modified record")
	}
	var patch Patch
	if e := json.Unmarshal([]byte(`{"end_date":null}`), &patch); e != nil {
		t.Fatal(e)
	}
	if e := patch.Apply(&s); e != nil || s.EndDate != nil {
		t.Fatal(e)
	}
	if e := (Patch{}).Apply(&s); e == nil {
		t.Fatal("empty patch accepted")
	}
}
func TestBoundedPeriod(t *testing.T) {
	if e := (Period{From: month(t, "01-2025"), To: month(t, "12-2025")}).Validate(); e != nil {
		t.Fatal(e)
	}
	for _, p := range []Period{{}, {From: month(t, "02-2025"), To: month(t, "01-2025")}, {From: month(t, "01-1900"), To: month(t, "01-2000")}} {
		if e := p.Validate(); e == nil {
			t.Fatal("bad period accepted")
		}
	}
}
func FuzzMonthRoundTrip(f *testing.F) {
	f.Add("07-2025")
	f.Add("invalid")
	f.Fuzz(func(t *testing.T, s string) {
		m, e := ParseMonth(s)
		if e != nil {
			return
		}
		if !m.Valid() {
			t.Fatal("invalid accepted month")
		}
		b, e := json.Marshal(m)
		if e != nil {
			t.Fatal(e)
		}
		var again Month
		if e := json.Unmarshal(b, &again); e != nil || again != m {
			t.Fatal("roundtrip failed")
		}
	})
}
