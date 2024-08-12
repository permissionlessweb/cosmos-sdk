package schema

// UniqueConstraint represents a unique constraint on a set of fields.
type UniqueConstraint struct {
	// FieldNames specifies a list of field names whose values must form a unique key across all objects of this type.
	FieldNames []string
}
