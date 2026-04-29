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

// compactSorted removes consecutive duplicates from a sorted slice and
// always returns a slice that does not alias the input — callers may
// freely append to the result without stomping the source's backing
// array.
//
// Bead mache-ad17c1: previous implementation aliased for len<=1, which
// matched the caller's current usage in mergeSortedDedup but was a
// latent hazard if the function gets reused.
func compactSorted(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	if len(s) == 1 {
		return []string{s[0]}
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
