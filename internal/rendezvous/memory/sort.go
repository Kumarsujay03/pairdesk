package memory

import "sort"

// sortBy is a small stable-sort helper so listings come back in a
// deterministic order even though the store keeps maps.
func sortBy[T any](xs []T, less func(a, b T) bool) {
	sort.SliceStable(xs, func(i, j int) bool { return less(xs[i], xs[j]) })
}
