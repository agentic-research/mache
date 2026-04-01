package graph

// mergeSortedDedup merges two sorted string slices into one sorted, deduplicated slice.
// Both inputs MUST already be sorted. Output is always sorted and contains no duplicates.
// Used by flushChildSlices to merge batches in O(n+m) instead of O((n+m) log(n+m)).
func mergeSortedDedup(a, b []string) []string {
	if len(a) == 0 {
		return compactSorted(b)
	}
	if len(b) == 0 {
		return compactSorted(a)
	}
	result := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			result = appendUniq(result, a[i])
			i++
		case a[i] > b[j]:
			result = appendUniq(result, b[j])
			j++
		default: // equal
			result = appendUniq(result, a[i])
			i++
			j++
		}
	}
	for ; i < len(a); i++ {
		result = appendUniq(result, a[i])
	}
	for ; j < len(b); j++ {
		result = appendUniq(result, b[j])
	}
	return result
}

// compactSorted removes consecutive duplicates from a sorted slice.
// Returns a new slice — does not mutate the input.
func compactSorted(s []string) []string {
	if len(s) <= 1 {
		return s
	}
	out := make([]string, 0, len(s))
	for _, v := range s {
		out = appendUniq(out, v)
	}
	return out
}

// appendUniq appends v only if it differs from the last element.
func appendUniq(s []string, v string) []string {
	if len(s) > 0 && s[len(s)-1] == v {
		return s
	}
	return append(s, v)
}
