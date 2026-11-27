package store

// LabelFilter matches a label's presence or its exact value.
type LabelFilter struct {
	Key, Value string
	HasValue   bool
}

// Matches reports whether labels contains the key and, when specified, its exact value.
func (f LabelFilter) Matches(labels map[string]string) bool {
	value, exists := labels[f.Key]
	return exists && (!f.HasValue || value == f.Value)
}
