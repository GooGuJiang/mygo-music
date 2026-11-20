package player

import "math"

type stereoBiquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             [2]float64
}

func (b *stereoBiquad) set(b0, b1, b2, a0, a1, a2 float64) {
	b.b0, b.b1, b.b2, b.a1, b.a2 = b0/a0, b1/a0, b2/a0, a1/a0, a2/a0
}

// The Audio EQ Cookbook formulas, with normalized coefficients and separate channel state.
func (b *stereoBiquad) peak(rate int, hz, q, db float64) {
	sine, cosine := math.Sincos(2 * math.Pi * hz / float64(rate))
	alpha := sine / (2 * q)
	a := math.Pow(10, db/40)
	b.set(1+alpha*a, -2*cosine, 1-alpha*a, 1+alpha/a, -2*cosine, 1-alpha/a)
}

func (b *stereoBiquad) shelf(rate int, hz, q, db float64, high bool) {
	sine, cosine := math.Sincos(2 * math.Pi * hz / float64(rate))
	a := math.Pow(10, db/40)
	beta := sine * math.Sqrt(a) / q
	plus, minus := a+1, a-1
	if high {
		b.set(a*(plus+minus*cosine+beta), -2*a*(minus+plus*cosine), a*(plus+minus*cosine-beta),
			plus-minus*cosine+beta, 2*(minus-plus*cosine), plus-minus*cosine-beta)
		return
	}
	b.set(a*(plus-minus*cosine+beta), 2*a*(minus-plus*cosine), a*(plus-minus*cosine-beta),
		plus+minus*cosine+beta, -2*(minus+plus*cosine), plus+minus*cosine-beta)
}

func (b *stereoBiquad) sample(ch int, x float64) float64 {
	y := b.b0*x + b.z1[ch]
	b.z1[ch] = b.b1*x - b.a1*y + b.z2[ch]
	b.z2[ch] = b.b2*x - b.a2*y
	return y
}
