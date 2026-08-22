package extensions

import (
	"fmt"
	"strconv"
	"strings"
)

// semver is a parsed MAJOR.MINOR.PATCH triple. Pre-release/build metadata
// (accepted by the manifest's semver pattern) are not compared here —
// dependency resolution orders by release version only; a capability
// dependency range never needs to distinguish "1.2.3-rc1" from "1.2.3".
type semver struct {
	major, minor, patch int
}

func parseSemver(s string) (semver, error) {
	s = strings.SplitN(s, "+", 2)[0]
	s = strings.SplitN(s, "-", 2)[0]
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("extensions: %q is not MAJOR.MINOR.PATCH", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return semver{}, fmt.Errorf("extensions: %q is not valid semver: %w", s, err)
		}
		nums[i] = n
	}
	return semver{nums[0], nums[1], nums[2]}, nil
}

// compare returns -1, 0, or 1 as a is less than, equal to, or greater than b.
func (a semver) compare(b semver) int {
	if a.major != b.major {
		return sign(a.major - b.major)
	}
	if a.minor != b.minor {
		return sign(a.minor - b.minor)
	}
	return sign(a.patch - b.patch)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// satisfiesRange reports whether version satisfies rangeExpr. Supported
// grammar (deliberately not full npm-semver-range — enough for extension
// dependency resolution without pulling a new module dependency):
//   - exact:                "1.2.3"
//   - single comparator:    ">=1.2.3", "<=1.2.3", ">1.2.3", "<1.2.3", "=1.2.3"
//   - caret (same major):   "^1.2.3"  ==  ">=1.2.3 <2.0.0" (or "<0.(minor+1).0" for major 0)
//   - space-separated AND:  ">=1.0.0 <2.0.0"
func satisfiesRange(version, rangeExpr string) (bool, error) {
	v, err := parseSemver(version)
	if err != nil {
		return false, err
	}
	for _, clause := range strings.Fields(rangeExpr) {
		ok, err := satisfiesClause(v, clause)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func satisfiesClause(v semver, clause string) (bool, error) {
	switch {
	case strings.HasPrefix(clause, "^"):
		base, err := parseSemver(clause[1:])
		if err != nil {
			return false, err
		}
		if v.compare(base) < 0 {
			return false, nil
		}
		if base.major > 0 {
			return v.major == base.major, nil
		}
		return v.major == 0 && v.minor == base.minor, nil
	case strings.HasPrefix(clause, ">="):
		base, err := parseSemver(clause[2:])
		if err != nil {
			return false, err
		}
		return v.compare(base) >= 0, nil
	case strings.HasPrefix(clause, "<="):
		base, err := parseSemver(clause[2:])
		if err != nil {
			return false, err
		}
		return v.compare(base) <= 0, nil
	case strings.HasPrefix(clause, ">"):
		base, err := parseSemver(clause[1:])
		if err != nil {
			return false, err
		}
		return v.compare(base) > 0, nil
	case strings.HasPrefix(clause, "<"):
		base, err := parseSemver(clause[1:])
		if err != nil {
			return false, err
		}
		return v.compare(base) < 0, nil
	case strings.HasPrefix(clause, "="):
		base, err := parseSemver(clause[1:])
		if err != nil {
			return false, err
		}
		return v.compare(base) == 0, nil
	default:
		base, err := parseSemver(clause)
		if err != nil {
			return false, err
		}
		return v.compare(base) == 0, nil
	}
}
