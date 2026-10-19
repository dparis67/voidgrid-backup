package safeint

import (
	"math"
	"testing"
)

func TestInt32(t *testing.T) {
	for in, want := range map[int]int32{
		0: 0, 1: 1, -1: -1,
		math.MaxInt32: math.MaxInt32, math.MaxInt32 + 1: math.MaxInt32,
		4294967297:    math.MaxInt32, // wraps to 1 with a plain cast
		math.MinInt32: math.MinInt32, math.MinInt32 - 1: math.MinInt32,
	} {
		if got := Int32(in); got != want {
			t.Errorf("Int32(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestInt64(t *testing.T) {
	for in, want := range map[uint64]int64{
		0: 0, 7: 7, math.MaxInt64: math.MaxInt64,
		math.MaxInt64 + 1: math.MaxInt64, math.MaxUint64: math.MaxInt64, // -1 with a plain cast
	} {
		if got := Int64(in); got != want {
			t.Errorf("Int64(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestMulBytes(t *testing.T) {
	cases := []struct {
		blocks uint64
		size   int64
		want   int64
	}{
		{0, 4096, 0},
		{1000, 4096, 4096000},
		{1000, 0, 0},
		{1000, -4096, 0},
		{math.MaxUint64, 4096, math.MaxInt64}, // a filesystem that reports all ones
		{math.MaxInt64/4096 + 1, 4096, math.MaxInt64},
		{math.MaxInt64 / 4096, 4096, math.MaxInt64 / 4096 * 4096},
	}
	for _, c := range cases {
		if got := MulBytes(c.blocks, c.size); got != c.want {
			t.Errorf("MulBytes(%d, %d) = %d, want %d", c.blocks, c.size, got, c.want)
		}
	}
}
