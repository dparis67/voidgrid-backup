// Package safeint converts between integer sizes without wrapping. A count or a
// size that does not fit becomes the nearest value that does, so an enormous
// number stays enormous instead of turning into a small or negative one.
package safeint

import "math"

// Int32 converts n to an int32, saturating at its limits.
func Int32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	}
	return int32(n)
}

// Int64 converts u to an int64, saturating at its largest value.
func Int64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// MulBytes is blocks times size, for a filesystem's block count and block
// size, saturating at the largest int64. A size that is not positive gives 0.
func MulBytes(blocks uint64, size int64) int64 {
	if size <= 0 {
		return 0
	}
	b := Int64(blocks)
	if b > math.MaxInt64/size {
		return math.MaxInt64
	}
	return b * size
}
