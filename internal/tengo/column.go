package tengo

import (
	"fmt"
	"strings"
)

// Column represents a single column of a table.
type Column struct {
	Name                string     `json:"name"`
	Type                ColumnType `json:"type"`
	Nullable            bool       `json:"nullable,omitempty"`
	AutoIncrement       bool       `json:"autoIncrement,omitempty"`
	Default             string     `json:"default,omitempty"` // Stored as an expression, i.e. quote-wrapped if string
	OnUpdate            string     `json:"onUpdate,omitempty"`
	GenerationExpr      string     `json:"generationExpression,omitempty"` // Only populated if generated column
	Virtual             bool       `json:"virtual,omitempty"`
	CharSet             string     `json:"charSet,omitempty"`       // Only populated if textual type
	Collation           string     `json:"collation,omitempty"`     // Only populated if textual type
	ShowCharSet         bool       `json:"showCharSet,omitempty"`   // Include CHARACTER SET in SHOW CREATE TABLE: always true if different than table default, sometimes true in other cases
	ShowCollation       bool       `json:"showCollation,omitempty"` // Include COLLATE in SHOW CREATE TABLE: logic differs by flavor
	Compression         string     `json:"compression,omitempty"`   // Only non-empty if using column compression in Percona Server or MariaDB
	Comment             string     `json:"comment,omitempty"`
	Invisible           bool       `json:"invisible,omitempty"` // True if an invisible column (MariaDB 10.3+, MySQL 8.0.23+)
	CheckClause         string     `json:"check,omitempty"`     // Only non-empty for MariaDB inline check constraint clause
	SpatialReferenceID  uint32     `json:"srid,omitempty"`      // Can be non-zero only for spatial types in MySQL 8+
	HasSpatialReference bool       `json:"has_srid,omitempty"`  // True if SRID attribute present; disambiguates SRID 0 vs no SRID
}

// Definition returns this column's definition clause, for use as part of a DDL
// statement.
func (c *Column) Definition(flavor Flavor) string {
	// At minimum, every column has a name and type. A number of different clauses
	// can follow those, with order varying a bit by flavor. We allocate a slice
	// with initial capacity of 6 somewhat arbitrarily: most columns will fit in
	// that, but we can do more allocations when needed for columns that exceed it
	clauses := make([]string, 2, 6)

	// Column name
	clauses[0] = EscapeIdentifier(c.Name)

	// Column data type
	if c.Compression != "" && flavor.IsMariaDB() {
		// MariaDB puts column compression modifier after column type
		clauses[1] = c.Type.String() + " " + flavor.compressedColumnOpenComment() + c.Compression + "*/"
	} else {
		clauses[1] = c.Type.String()
	}

	// Character set and collation
	if c.CharSet != "" && c.ShowCharSet {
		clauses = append(clauses, "CHARACTER SET "+c.CharSet)
	}
	if c.Collation != "" && c.ShowCollation {
		clauses = append(clauses, "COLLATE "+c.Collation)
	}

	// Generation expression
	if c.GenerationExpr != "" {
		var genKind string
		if c.Virtual {
			genKind = "VIRTUAL"
		} else {
			genKind = "STORED"
		}
		clauses = append(clauses, "GENERATED ALWAYS AS ("+c.GenerationExpr+") "+genKind)
	}

	// Nullability
	if !c.Nullable {
		clauses = append(clauses, "NOT NULL")
	} else if c.Type.Base == "timestamp" {
		// Oddly the timestamp type always displays nullability, other types never do
		clauses = append(clauses, "NULL")
	}

	// SRID in MySQL 8.0+
	if c.HasSpatialReference && flavor.MinMySQL(8) {
		// Although MariaDB also attribute syntax for this (REF_SYSTEM_ID), it isn't
		// exposed in SHOW CREATE TABLE, so here we restrict to MySQL only
		clauses = append(clauses, fmt.Sprintf("/*!80003 SRID %d */", c.SpatialReferenceID))
	}

	// Invisibility for MariaDB 10.3-11.6 (which places this in a different spot than MySQL or MariaDB 11.7+)
	if c.Invisible && flavor.IsMariaDB() && !flavor.MinMariaDB(11, 7) {
		clauses = append(clauses, "INVISIBLE")
	}

	// Column compression in Percona Server
	if c.Compression != "" && flavor.IsPercona() {
		clauses = append(clauses, flavor.compressedColumnOpenComment()+"COLUMN_FORMAT "+c.Compression+" */")
	}

	// Default value/expression
	if c.Default != "" {
		clauses = append(clauses, "DEFAULT "+c.Default)
	}

	// ON UPDATE for TIMESTAMP or DATETIME
	if c.OnUpdate != "" {
		clauses = append(clauses, "ON UPDATE "+c.OnUpdate)
	}

	// Auto increment
	if c.AutoIncrement {
		clauses = append(clauses, "AUTO_INCREMENT")
	}

	// Invisibility for MySQL, or MariaDB 11.7+ which moves it to this spot
	if c.Invisible {
		if flavor.IsMySQL() {
			clauses = append(clauses, "/*!80023 INVISIBLE */")
		} else if flavor.MinMariaDB(11, 7) { // changed in MDEV-35308
			clauses = append(clauses, "INVISIBLE")
		}
	}

	// Column comment
	if c.Comment != "" {
		clauses = append(clauses, "COMMENT '"+EscapeValueForCreateTable(c.Comment)+"'")
	}

	// Inline CHECK constraint (field is only ever set for MariaDB)
	if c.CheckClause != "" {
		clauses = append(clauses, "CHECK ("+c.CheckClause+")")
	}

	return strings.Join(clauses, " ")
}

// Equals returns true if two columns are identical, false otherwise.
func (c *Column) Equals(other *Column) bool {
	// shortcut if both nil pointers, or both pointing to same underlying struct
	if c == other {
		return true
	}
	// if one is nil, but we already know the two aren't equal, then we know the other is non-nil
	if c == nil || other == nil {
		return false
	}
	// Just compare the fields, they're all simple non-pointer scalars. This does
	// intentionally treat two columns as different if they only differ in
	// cosmetic / non-functional ways; see Column.Equivalent() below for a looser
	// comparison.
	return *c == *other
}

// Equivalent returns true if two columns are equal, or only differ in cosmetic/
// non-functional ways. Cosmetic differences can come about in MySQL 8 when a
// column was created with CHARACTER SET or COLLATION clauses that are
// unnecessary (equal to table's default); or when comparing a table across
// different versions of MySQL 8 (one which supports int display widths, and
// one that removes them).
// Note that, for the purposes of this method, column comments are NOT
// considered cosmetic. This method returns false if c and other only differ
// by a comment.
func (c *Column) Equivalent(other *Column) bool {
	// If they're equal, they're also equivalent
	if c.Equals(other) {
		return true
	}
	// if one is nil, but we already know the two aren't equal, then we know the other is non-nil
	if c == nil || other == nil {
		return false
	}

	// Compare column types. This ignores differences in only the *presence/lack*
	// of int display widths, which is a cosmetic difference that can come up
	// across flavors. Any other difference (including *changing* an int display
	// width) is functional.
	if !c.Type.Equivalent(other.Type) {
		return false
	}
	// If we didn't return early, we know either Type didn't change at all, or
	// it only differs in a cosmetic manner.

	// Make a copy of c, and make all cosmetic-related fields equal to other's, and
	// then check equality again to determine equivalence.
	selfCopy := *c
	selfCopy.Type = other.Type
	selfCopy.ShowCharSet = other.ShowCharSet
	selfCopy.ShowCollation = other.ShowCollation
	if charsetsEquivalent(c.CharSet, other.CharSet) {
		selfCopy.CharSet = other.CharSet
	}
	if collationsEquivalent(c.Collation, other.Collation) {
		selfCopy.Collation = other.Collation
	}
	return selfCopy == *other
}

func charsetsEquivalent(a, b string) bool {
	// Account for flavor differences in how utf8mb3 is expressed
	return (a == b) || (a == "utf8mb3" && b == "utf8") || (a == "utf8" && b == "utf8mb3")
}

func collationsEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	// Account for flavor differences in how utf8mb3's collations are expressed
	aCharset, aRest, _ := strings.Cut(a, "_")
	bCharset, bRest, _ := strings.Cut(b, "_")
	if aRest != bRest {
		return false
	}
	return (aCharset == "utf8" && bCharset == "utf8mb3") || (aCharset == "utf8mb3" && bCharset == "utf8")
}
