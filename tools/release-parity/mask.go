package main

import "github.com/google/go-cmp/cmp"

// runsPerSide is how many times each binary runs the plan to measure its own
// noise. Two runs missed a timing race that happened to agree within each side
// (the sheaf subscriber reconnecting); three makes that much rarer, and a
// difference that survives is reported, never hidden.
const runsPerSide = 3

// noise is what a masked value is replaced with.
const noise = "<varies between identical runs>"

// maskNoise returns first with every value that differs from second masked.
func maskNoise(first, second []answer) []answer {
	out := make([]answer, len(first))
	for i := range first {
		out[i] = first[i]
		out[i].Body = mask(first[i].Body, second[i].Body)
	}
	return out
}

// mask returns a, with every leaf (or shape) that differs from b replaced by
// [noise]. Objects are compared key by key and arrays index by index when
// their lengths agree; anything else that differs is masked whole.
func mask(a, b any) any {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return noise
		}
		out := make(map[string]any, len(av))
		for k, v := range av {
			w, ok := bv[k]
			if !ok {
				out[k] = noise
				continue
			}
			out[k] = mask(v, w)
		}
		// A key only the later run has is noise too: its presence varies.
		for k := range bv {
			if _, ok := av[k]; !ok {
				out[k] = noise
			}
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok || len(bv) != len(av) {
			if cmp.Equal(a, b) {
				return a
			}
			return noise
		}
		out := make([]any, len(av))
		for i := range av {
			out[i] = mask(av[i], bv[i])
		}
		return out
	default:
		if cmp.Equal(a, b) {
			return a
		}
		return noise
	}
}

// maskEither masks, in both a and b, every position either side already
// masked, so a field that is noise for one version is not reported as a
// difference.
func maskEither(a, b any) (any, any) {
	if a == noise || b == noise {
		return noise, noise
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		ao, bo := make(map[string]any, len(am)), make(map[string]any, len(bm))
		for k, v := range am {
			ao[k] = v
		}
		for k, v := range bm {
			bo[k] = v
		}
		for k := range am {
			if w, ok := bm[k]; ok {
				ao[k], bo[k] = maskEither(am[k], w)
			}
		}
		// A key that is noise on one side and absent on the other is a field
		// whose PRESENCE varies between identical runs; drop it from both.
		for k, v := range ao {
			if _, ok := bo[k]; !ok && v == noise {
				delete(ao, k)
			}
		}
		for k, v := range bo {
			if _, ok := ao[k]; !ok && v == noise {
				delete(bo, k)
			}
		}
		return ao, bo
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok && len(as) == len(bs) {
		ao, bo := make([]any, len(as)), make([]any, len(bs))
		for i := range as {
			ao[i], bo[i] = maskEither(as[i], bs[i])
		}
		return ao, bo
	}
	return a, b
}
