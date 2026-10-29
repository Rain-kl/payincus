// Package models holds Go structs generated from server/prisma/schema.prisma plus
// the hand-written helpers they depend on.
package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// Decimal mirrors Prisma's Decimal scalar. Prisma serializes Decimal to a JSON
// string (e.g. "100.00"), so this type marshals as a quoted string to keep API
// responses byte-compatible with the Node backend.
type Decimal struct {
	decimal.Decimal
}

// NewDecimalFromString parses a decimal literal.
func NewDecimalFromString(value string) (Decimal, error) {
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		return Decimal{}, err
	}
	return Decimal{parsed}, nil
}

// MarshalJSON renders the decimal as a JSON string, matching Prisma.
func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts either a JSON string or number.
func (d *Decimal) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		d.Decimal = decimal.Zero
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		parsed, err := decimal.NewFromString(asString)
		if err != nil {
			return err
		}
		d.Decimal = parsed
		return nil
	}
	parsed, err := decimal.NewFromString(string(data))
	if err != nil {
		return fmt.Errorf("models.Decimal: cannot parse %s", string(data))
	}
	d.Decimal = parsed
	return nil
}

// Value implements driver.Valuer for numeric columns.
func (d Decimal) Value() (driver.Value, error) {
	return d.Decimal.Value()
}

// Scan implements sql.Scanner for numeric columns.
func (d *Decimal) Scan(value any) error {
	if value == nil {
		d.Decimal = decimal.Zero
		return nil
	}
	return d.Decimal.Scan(value)
}

// GormDataType tells GORM to use the PostgreSQL numeric type.
func (Decimal) GormDataType() string { return "numeric" }
