package access

import "slices"

// RuleMethods are the methods the rules table names, sorted.
func RuleMethods() []string {
	out := make([]string, 0, len(rules))
	for m := range rules {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}
