package qcowstream

import (
	"fmt"
	"math/bits"
)

func add64(a, b uint64) (uint64, error) {
	s, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return s, nil
}

func mul64(a, b uint64) (uint64, error) {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return 0, fmt.Errorf("%w: %d * %d", ErrOverflow, a, b)
	}
	return lo, nil
}

// ceilDiv returns ceil(a/b) for b > 0, without overflowing on a+b-1.
func ceilDiv(a, b uint64) uint64 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}

// sum adds all values, failing on overflow.
func sum(vs ...uint64) (uint64, error) {
	var t uint64
	var err error
	for _, v := range vs {
		if t, err = add64(t, v); err != nil {
			return 0, err
		}
	}
	return t, nil
}
