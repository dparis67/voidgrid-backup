package server

import "unicode/utf8"

// spillNames decides, for each bar of a lane, whether its name fits inside it.
// A name that does not is written beside the bar (Spill is "right" or "left"),
// as long as the space there is free: no other bar in the same row and no
// other name already placed there. If neither side is free the name stays
// clipped inside the bar, and the hover text has it.
func spillNames(bars []tlBar) {
	type span struct{ l, r float64 }
	pctOf := func(px float64) float64 { return px / spillTrackPx * 100 }
	occupied := map[int][]span{} // row -> the stretches of it already in use
	effective := func(b tlBar) float64 { return max(b.Width, pctOf(minBarPx)) }
	for _, b := range bars {
		occupied[b.Row] = append(occupied[b.Row], span{b.Left, b.Left + effective(b)})
	}
	free := func(row int, s span) bool {
		if s.l < 0 || s.r > 100 {
			return false
		}
		for _, o := range occupied[row] {
			if s.l < o.r && o.l < s.r {
				return false
			}
		}
		return true
	}
	for i := range bars {
		b := &bars[i]
		text := pctOf(float64(utf8.RuneCountInString(b.Name)) * spillCharPx)
		eff := effective(*b)
		if eff >= text+pctOf(minBarPx) {
			continue // it fits inside
		}
		gap := pctOf(spillGapPx)
		right := span{b.Left + eff + gap, b.Left + eff + gap + text}
		left := span{b.Left - gap - text, b.Left - gap}
		switch {
		case free(b.Row, right):
			b.Spill = "right"
			occupied[b.Row] = append(occupied[b.Row], right)
		case free(b.Row, left):
			b.Spill = "left"
			occupied[b.Row] = append(occupied[b.Row], left)
		}
	}
}
