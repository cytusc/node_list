package domain

import (
	"bytes"
	"testing"
)

// sign normalises a comparison result to -1, 0 or 1.
func sign(v int) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	default:
		return 0
	}
}

// The expectations below were captured from Node 22 with
// `a.localeCompare(b, "zh-CN", {numeric: true})`. They pin the ICU behaviour
// that golang.org/x/text/collate.Numeric gets wrong.
func TestCompareNumericMatchesICU(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		// Digit runs compare by value.
		{"a1", "a2", -1},
		{"a2", "a10", -1},
		{"x9y", "x10y", -1},
		{"9", "10", -1},
		{"09", "10", -1},
		// Leading zeros carry no weight.
		{"0", "01", -1},
		{"0", "00", 0},
		{"1", "01", 0},
		{"a01", "a1", 0},
		{"009", "9", 0},
		{"a01", "a2", -1},
		// A number sorts after punctuation and before letters.
		{"a1", "ab", -1},
		{"1", "a", -1},
		{"1", "A", -1},
		{"0", "a", -1},
		{"D0A7 D+ [Qilan]", "D8003D D+ [Qilan]", -1},
		{"0EEFFB D+ [Qilan]", "9 [Qilan]", -1},
		// A shorter string sorts first when the remainder is a number.
		{"a", "a0", -1},
		{"a1", "a", 1},
		// Tertiary differences are only reached once the primaries tie.
		{"a", "A", -1},
		{"A", "a0", -1},
		{"a1", "A", 1},
		{"z", "Z", -1},
		{"a", "b", -1},
		{"a1b", "a1c", -1},
	}
	for _, tc := range cases {
		t.Run(tc.left+"|"+tc.right, func(t *testing.T) {
			if got := CompareNumeric(tc.left, tc.right); got != tc.want {
				t.Errorf("CompareNumeric(%q, %q) = %d, want %d",
					tc.left, tc.right, got, tc.want)
			}
			if got := CompareNumeric(tc.right, tc.left); got != -tc.want {
				t.Errorf("CompareNumeric(%q, %q) = %d, want %d",
					tc.right, tc.left, got, -tc.want)
			}
		})
	}
}

// numericEqual reports whether two values tie under the comparator.
func numericEqual(t *testing.T, a, b string) bool {
	t.Helper()
	return CompareNumeric(a, b) == 0 && CompareNumeric(b, a) == 0
}

func TestCompareNumericZeroSpellings(t *testing.T) {
	for _, zero := range []string{"0", "00", "000", "0000"} {
		if !numericEqual(t, zero, "0") {
			t.Errorf("%q should compare equal to \"0\"", zero)
		}
	}
}

func TestNumericTransform(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"abc":     "abc",
		"a1":      "a0011",
		"a10":     "a00210",
		"a01":     "a0011",
		"a0":      "a0010",
		"00":      "0010",
		"1a2":     "0011a0012",
		"US 9":    "US 0019",
		"x10y":    "x00210y",
		"a·1":     "a·0011",
		"1000000": "0071000000",
	}
	for in, want := range cases {
		if got := numericTransform(in); got != want {
			t.Errorf("numericTransform(%q) = %q, want %q", in, got, want)
		}
	}
}

// SortKey must order exactly like CompareNumeric; the PostgreSQL projection
// ranks columns with ORDER BY on these bytes.
func TestSortKeyAgreesWithCompareNumeric(t *testing.T) {
	values := []string{
		"", "0", "00", "01", "1", "1·a", "1a", "01a", "9", "09", "009", "9z",
		"10", "a", "A", "a-1", "a·1", "a0", "a1", "a01", "a1-", "a1b", "a1c",
		"a2", "a10", "ab", "AE 07 D+", "AE 17", "US D0A7 D+ [Qilan]",
		"US D8003D D+ [Qilan]", "US 0EEFFB D+ [Qilan]", "US 9 [Qilan]", "z", "Z",
	}
	for _, a := range values {
		for _, b := range values {
			byCompare := CompareNumeric(a, b)
			byKey := bytes.Compare(SortKey(a), SortKey(b))
			if sign(byCompare) != sign(byKey) {
				t.Errorf("SortKey disagrees with CompareNumeric for %q vs %q: %d vs %d",
					a, b, byCompare, byKey)
			}
		}
	}
}

// TestCompareNumericRegressionProductionNames pins the names that the x/text
// numeric collator ordered differently from ICU in production data.
func TestCompareNumericRegressionProductionNames(t *testing.T) {
	// x/text put "D8003D" before "D0A7" and "9" before "0EEFFB".
	if CompareNumeric("US D0A7 D+ [Qilan]", "US D8003D D+ [Qilan]") >= 0 {
		t.Error(`"D0A7" must sort before "D8003D"`)
	}
	if CompareNumeric("US 0EEFFB D+ [Qilan]", "US 9 [Qilan]") >= 0 {
		t.Error(`"0EEFFB" must sort before "9"`)
	}
	// x/text decided on tertiary weight before reaching the trailing number.
	if CompareNumeric("A", "a0") >= 0 {
		t.Error(`"A" must sort before "a0"`)
	}
	if CompareNumeric("A", "a10") >= 0 {
		t.Error(`"A" must sort before "a10"`)
	}
}
