package payoutinstrument

import (
	"regexp"
	"strings"
)

// panRun matches a maximal run of digits joined by single spaces or hyphens
// (how a card number is typed).
var panRun = regexp.MustCompile(`[0-9](?:[ -]?[0-9])*`)

// ContainsPAN reports whether s contains a Luhn-valid string of 12-19 digits
// (ADR 0111 2.9, L-8): the only way to be sure a primary account number never
// reaches storage is to refuse anything that could be one. The unit is the
// MAXIMAL digit run (separators allowed between digits); a longer run is not
// split into windows (that would refuse most IBANs).
func ContainsPAN(s string) bool {
	for _, run := range panRun.FindAllString(s, -1) {
		d := strings.NewReplacer(" ", "", "-", "").Replace(run)
		if len(d) >= 12 && len(d) <= 19 && luhnValid(d) {
			return true
		}
	}
	return false
}

func luhnValid(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
