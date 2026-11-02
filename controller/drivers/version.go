package drivers

import "strings"

// CompareVersions orders Debian package versions ([epoch:]upstream[-revision])
// the way dpkg does: <0 when a is older than b, 0 when equal, >0 when newer.
func CompareVersions(a, b string) int {
	ea, ua, ra := splitVersion(a)
	eb, ub, rb := splitVersion(b)
	if c := compareNum(ea, eb); c != 0 {
		return c
	}
	if c := comparePart(ua, ub); c != 0 {
		return c
	}
	return comparePart(ra, rb)
}

func splitVersion(v string) (epoch, upstream, revision string) {
	if e, rest, ok := strings.Cut(v, ":"); ok {
		epoch, v = e, rest
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

// comparePart alternates between non-digit runs (compared with dpkg's
// character order, where ~ sorts before everything, even the end) and digit
// runs (compared as numbers).
func comparePart(a, b string) int {
	for a != "" || b != "" {
		var na, nb string
		na, a = span(a, false)
		nb, b = span(b, false)
		if c := compareText(na, nb); c != 0 {
			return c
		}
		na, a = span(a, true)
		nb, b = span(b, true)
		if c := compareNum(na, nb); c != 0 {
			return c
		}
	}
	return 0
}

func span(s string, digits bool) (string, string) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') == digits {
		i++
	}
	return s[:i], s[i:]
}

func order(c byte, ok bool) int {
	switch {
	case !ok:
		return 0
	case c == '~':
		return -1
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return int(c)
	default:
		return int(c) + 256
	}
}

func compareText(a, b string) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var ca, cb byte
		if i < len(a) {
			ca = a[i]
		}
		if i < len(b) {
			cb = b[i]
		}
		if oa, ob := order(ca, i < len(a)), order(cb, i < len(b)); oa != ob {
			return oa - ob
		}
	}
	return 0
}

func compareNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}
