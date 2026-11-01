package raop

import "testing"

func TestAirplayVolumeRange(t *testing.T) {
	for _, tc := range []struct{ slider, db float64 }{{0, -144}, {.01, -29.7}, {.03, -29.1}, {.04, -28.8}, {.5, -15}, {1, 0}} {
		if got := airplayVolumeDB(tc.slider); got-tc.db > 0.0001 || tc.db-got > 0.0001 {
			t.Errorf("slider %v: got %v want %v", tc.slider, got, tc.db)
		}
	}
}
