package domain

import (
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// JavaScript sorts every text column with
// String.prototype.localeCompare(other, "zh-CN", {numeric: true}).
//
// golang.org/x/text/collate cannot reproduce that comparator. Its numeric
// weigher (verified in v0.43.0, internal/colltab/numeric.go) brackets a run with
// a "number start" primary weight placed just after the weight for "9", and then
// encodes the run's *digit count* as a low primary weight rather than its value.
// Two consequences, both measured against Node 22 on the production corpus,
// where x/text disagreed on 230 of 904 node names:
//
//   - a lone leading zero is dropped, so "0EEFFB" contributes no primary weight
//     and the following letter is compared against "9"'s digit count: x/text
//     reports "9" < "0EEFFB", ICU reports "0EEFFB" < "9";
//   - the digit count orders before the digits, so x/text reports
//     "D8003D" < "D0A7", the opposite of ICU's 0 < 8003.
//
// ICU instead orders a run by value while keeping it inside the digit range,
// which sits after punctuation and before letters. Rewriting each run as
// "<digits><value>" with a fixed-width digit count reproduces that exactly: the
// plain zh-CN collator then puts numbers after punctuation and before letters,
// and compares digit-by-digit within a run.
//
// The rewrite happens on the whole string rather than per token so the collator
// still compares level by level. Comparing runs in isolation would decide at
// the tertiary level too early — it would order "A" after "a10" instead of after
// "a", because "A" vs "a" alone differs only in tertiary weight.

// numericRunWidth is the fixed width of the digit-count prefix. It places a run
// of d digits before any run of more than d digits, so nine-digit and longer
// values still order by magnitude. Runs longer than nine hundred and
// ninety-nine digits clamp, which no value in this service reaches.
const numericRunWidth = 3

var (
	plainMu   sync.Mutex
	plainColl = collate.New(language.Chinese)
	keyPool   = sync.Pool{New: func() any { return new(collate.Buffer) }}
)

// CompareStrings orders two strings the way Intl.Collator("zh-CN") does.
func CompareStrings(a, b string) int {
	plainMu.Lock()
	defer plainMu.Unlock()
	return plainColl.CompareString(a, b)
}

// CompareNumeric orders two strings the way
// localeCompare(other, "zh-CN", {numeric: true}) does.
func CompareNumeric(a, b string) int {
	return CompareStrings(numericTransform(a), numericTransform(b))
}

// SortKey returns a byte-comparable key with the same ordering as
// CompareNumeric, so the PostgreSQL projection can rank a column with ORDER BY
// instead of re-collating every comparison — the cost that made the JavaScript
// implementation slow.
func SortKey(s string) []byte {
	buf := keyPool.Get().(*collate.Buffer)
	defer keyPool.Put(buf)
	plainMu.Lock()
	key := plainColl.KeyFromString(buf, numericTransform(s))
	plainMu.Unlock()
	out := make([]byte, len(key))
	copy(out, key)
	return out
}

// numericTransform rewrites every run of decimal digits into a fixed-width
// digit count followed by the digits with leading zeros removed, so that plain
// collation compares the runs by value.
func numericTransform(s string) string {
	if !hasDigit(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for s != "" {
		digits, rest := cutDigits(s)
		if digits == "" {
			text, tail := cutText(s)
			b.WriteString(text)
			s = tail
			continue
		}
		writeNumericRun(&b, digits)
		s = rest
	}
	return b.String()
}

// writeNumericRun appends the canonical form of one digit run.
func writeNumericRun(b *strings.Builder, digits string) {
	significant := strings.TrimLeft(digits, "0")
	if significant == "" {
		// Every spelling of zero compares equal: "0", "00", "000".
		significant = "0"
	}
	length := len(significant)
	if length > 999 {
		significant, length = significant[:999], 999
	}
	encoded := strconv.Itoa(length)
	b.WriteString(strings.Repeat("0", numericRunWidth-len(encoded)))
	b.WriteString(encoded)
	b.WriteString(significant)
}

// hasDigit reports whether the string contains a digit, which lets the common
// no-digit case skip the rewrite entirely.
func hasDigit(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsDigit(r) {
			return true
		}
		i += size
	}
	return false
}

// cutDigits splits off a leading run of decimal digits.
func cutDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsDigit(r) {
			break
		}
		i += size
	}
	return s[:i], s[i:]
}

// cutText splits off a leading run of non-digits.
func cutText(s string) (text, rest string) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsDigit(r) {
			break
		}
		i += size
	}
	return s[:i], s[i:]
}
