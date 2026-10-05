package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrNotFound    = errors.New("subscription not found")
	ErrConflict    = errors.New("idempotency key reused with different data")
	ErrVersion     = errors.New("resource version changed")
	ErrUnavailable = errors.New("shard unavailable or topology changed")
	ErrQuota       = errors.New("subscription quota exceeded")
)

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }
func Invalid(s string) error             { return &ValidationError{s} }

// Month is an integer month index; JSON/API uses MM-YYYY and SQL uses DATE.
// Range is 0001-01 through 9999-12; the zero value is deliberately invalid.
type Month int32

func ParseMonth(s string) (Month, error) {
	if len(s) != 7 || s[2] != '-' {
		return 0, Invalid("date must use MM-YYYY")
	}
	t, e := time.Parse("01-2006", s)
	if e != nil || t.Year() < 1 || t.Year() > 9999 {
		return 0, Invalid("invalid calendar month")
	}
	return Month((t.Year()-1)*12 + int(t.Month())), nil
}
func (m Month) Valid() bool { return m >= 1 && m <= 9999*12 }
func (m Month) Time() time.Time {
	return time.Date((int(m)-1)/12+1, time.Month((int(m)-1)%12+1), 1, 0, 0, 0, 0, time.UTC)
}
func (m Month) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("invalid month")
	}
	return json.Marshal(m.Time().Format("01-2006"))
}
func (m *Month) UnmarshalJSON(b []byte) error {
	var s string
	if e := json.Unmarshal(b, &s); e != nil {
		return Invalid("date must be a string")
	}
	v, e := ParseMonth(s)
	if e != nil {
		return e
	}
	*m = v
	return nil
}
func FromTime(t time.Time) Month { return Month((t.Year()-1)*12 + int(t.Month())) }

type OptionalMonth struct {
	Set   bool
	Value *Month
}

func (o *OptionalMonth) UnmarshalJSON(b []byte) error {
	var v *Month
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	o.Set = true
	o.Value = v
	return nil
}

type Subscription struct {
	ID          uuid.UUID `json:"id"`
	UserID      uuid.UUID `json:"user_id"`
	ServiceName string    `json:"service_name"`
	Price       int64     `json:"price"`
	StartDate   Month     `json:"start_date"`
	EndDate     *Month    `json:"end_date"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type Create struct {
	ServiceName string `json:"service_name"`
	Price       int64  `json:"price"`
	StartDate   Month  `json:"start_date"`
	EndDate     *Month `json:"end_date"`
}
type Patch struct {
	ServiceName *string       `json:"service_name"`
	Price       *int64        `json:"price"`
	StartDate   *Month        `json:"start_date"`
	EndDate     OptionalMonth `json:"end_date"`
}
type List struct {
	Limit     int
	Name      string
	AfterTime time.Time
	AfterID   uuid.UUID
}
type Period struct {
	From, To Month
	Name     string
}

func ValidName(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > 255 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return strings.TrimSpace(s) != ""
}
func (c *Create) Validate() error {
	c.ServiceName = strings.TrimSpace(c.ServiceName)
	if !ValidName(c.ServiceName) {
		return Invalid("service_name must contain 1..255 characters without controls")
	}
	if c.Price < 1 || c.Price > 2147483647 {
		return Invalid("price must be an integer in 1..2147483647")
	}
	if !c.StartDate.Valid() {
		return Invalid("start_date is required")
	}
	if c.EndDate != nil && (!c.EndDate.Valid() || *c.EndDate < c.StartDate) {
		return Invalid("end_date must be on or after start_date")
	}
	return nil
}
func (p Patch) Apply(s *Subscription) error {
	if p.ServiceName == nil && p.Price == nil && p.StartDate == nil && !p.EndDate.Set {
		return Invalid("at least one patch field is required")
	}
	c := Create{s.ServiceName, s.Price, s.StartDate, s.EndDate}
	if p.ServiceName != nil {
		c.ServiceName = *p.ServiceName
	}
	if p.Price != nil {
		c.Price = *p.Price
	}
	if p.StartDate != nil {
		c.StartDate = *p.StartDate
	}
	if p.EndDate.Set {
		c.EndDate = p.EndDate.Value
	}
	if e := c.Validate(); e != nil {
		return e
	}
	s.ServiceName = c.ServiceName
	s.Price = c.Price
	s.StartDate = c.StartDate
	s.EndDate = c.EndDate
	return nil
}
func (p Period) Validate() error {
	if !p.From.Valid() || !p.To.Valid() || p.To < p.From || p.To-p.From >= 1200 {
		return Invalid("period must span 1..1200 months, from <= to")
	}
	if p.Name != "" && !ValidName(p.Name) {
		return Invalid("invalid service_name")
	}
	return nil
}
