package domain

import (
	"sort"
	"strings"
)

// This file reproduces the WHATWG URL serialisation the JavaScript service
// relied on. The rules were derived empirically against Node 22 rather than
// from the specification, because the two disagree in places that matter:
//
//   - userinfo and path are escaped once at parse time and then emitted
//     verbatim. Percent escapes already present in the link - valid or not,
//     upper or lower case - survive untouched, so decoding them first and
//     re-escaping produces a different string.
//   - the query is the exception: URLSearchParams.sort() always re-serialises
//     it, so it is decoded into pairs and then rendered with the
//     application/x-www-form-urlencoded serializer (space becomes "+", "/"
//     becomes %2F, "=" becomes %3D ...).
//   - only "special" schemes (http, https, ws, wss, ftp, file) lower-case the
//     host and drop the default port. vless/vmess/trojan/ss/hysteria2 hosts are
//     opaque and keep their original case.
//
// Getting any of this wrong rewrites every identity in the database, which
// would orphan stored IPPure results and retire the whole fleet, so the
// constants below are byte-for-byte copies of measured output.

// ":" is NOT safe here: the parser spends the first colon on the
// username/password separator and the serializer writes that one back, but any
// further colon is escaped ("a:b:c@h" becomes "a:b%3Ac@h").
const userinfoSafeBytes = "!$%&'()*+,-.0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"_abcdefghijklmnopqrstuvwxyz~"

// Path escaping for non-special schemes. "\" "[" "]" "^" "|" ";" "=" ":" "@"
// all survive here, which is why the transport settings carried in a vless
// path stay readable.
const pathSafeBytes = "!$%&'()*+,-./0123456789:;=@ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"[]\\^_abcdefghijklmnopqrstuvwxyz|~"

// Path escaping for special schemes is the same set without "\", which the
// serializer turns into "/".
const specialPathSafeBytes = "!$%&'()*+,-./0123456789:;=@ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"[]^_abcdefghijklmnopqrstuvwxyz|~"

// opaquePathSafeBytes is used when a link has no authority at all.
const opaquePathSafeBytes = pathSafeBytes

var specialSchemes = map[string]string{
	"http":  "80",
	"https": "443",
	"ws":    "80",
	"wss":   "443",
	"ftp":   "21",
	"file":  "",
}

// whatwgSerialize renders a share link the way new URL(link) +
// searchParams.sort() + toString() does. It reports false when the JavaScript
// parser would have thrown, so the caller can fall back to the summary
// identity just like the original catch block did.
func whatwgSerialize(link string) (string, bool) {
	// The fragment is cleared before serialisation.
	if i := strings.IndexByte(link, '#'); i >= 0 {
		link = link[:i]
	}
	colon := strings.IndexByte(link, ':')
	if colon <= 0 {
		return "", false
	}
	scheme := strings.ToLower(link[:colon])
	if scheme == "hy2" {
		scheme = "hysteria2"
	}
	_, special := specialSchemes[scheme]
	rest := link[colon+1:]
	if !strings.HasPrefix(rest, "//") {
		// No authority: the remainder is an opaque path.
		return scheme + ":" + escapeWhatWG(rest, opaquePathSafeBytes), true
	}
	rest = rest[2:]

	// The authority ends at the first "/" or "?".
	end := len(rest)
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		end = i
	}
	authority, remainder := rest[:end], rest[end:]

	pathRaw, queryRaw := "", ""
	hasQuery := false
	if strings.HasPrefix(remainder, "?") {
		queryRaw, hasQuery = remainder[1:], true
	} else if i := strings.IndexByte(remainder, '?'); i >= 0 {
		pathRaw, queryRaw, hasQuery = remainder[:i], remainder[i+1:], true
	} else {
		pathRaw = remainder
	}

	// The authority splits at the LAST "@": every earlier one is escaped into
	// the userinfo ("a@b@c" is user "a%40b" at host "c").
	userinfo, hostport := "", authority
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo, hostport = authority[:i], authority[i+1:]
	}
	// JavaScript rejects an empty host and falls back to the summary identity.
	host, port, ok := splitHostPort(hostport)
	if !ok || host == "" {
		return "", false
	}
	hostOut := host
	if special {
		hostOut = strings.ToLower(host)
		if port != "" && port == specialSchemes[scheme] {
			port = ""
		}
	}
	if port != "" {
		hostOut += ":" + port
	}

	pathSafe := pathSafeBytes
	if special {
		pathSafe = specialPathSafeBytes
	}

	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	if userinfo != "" {
		// The first ":" separates username from password; the serializer writes
		// it back verbatim.
		user, password := userinfo, ""
		hasPassword := false
		if i := strings.IndexByte(userinfo, ':'); i >= 0 {
			user, password, hasPassword = userinfo[:i], userinfo[i+1:], true
		}
		b.WriteString(escapeWhatWG(user, userinfoSafeBytes))
		if hasPassword {
			b.WriteByte(':')
			b.WriteString(escapeWhatWG(password, userinfoSafeBytes))
		}
		b.WriteByte('@')
	}
	b.WriteString(hostOut)
	b.WriteString(escapeWhatWG(pathRaw, pathSafe))
	if hasQuery {
		if query := encodeURLQuery(queryRaw); query != "" {
			b.WriteByte('?')
			b.WriteString(query)
		}
	}
	return b.String(), true
}

// splitHostPort splits "host", "host:port" or "[v6]:port" the way the URL
// parser does: a bracketed IPv6 literal ends at "]", anything else ends at the
// first ":". It reports false for hosts JavaScript refuses.
func splitHostPort(hostport string) (host, port string, ok bool) {
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", "", false
		}
		host = hostport[:end+1]
		tail := hostport[end+1:]
		if tail == "" {
			return host, "", true
		}
		if tail[0] != ':' {
			return "", "", false
		}
		return host, tail[1:], true
	}
	// A second ":" means an unbracketed IPv6 literal, which JavaScript rejects.
	first := strings.IndexByte(hostport, ':')
	if first < 0 {
		return hostport, "", true
	}
	if strings.IndexByte(hostport[first+1:], ':') >= 0 {
		return "", "", false
	}
	for _, c := range []byte(hostport) {
		if c <= 0x20 || c == '"' || c == '<' || c == '>' || c == '^' ||
			c == '|' || c == '\\' || c == '{' || c == '}' {
			return "", "", false
		}
	}
	return hostport[:first], hostport[first+1:], true
}

type queryPair struct {
	key   string
	value string
}

// encodeURLQuery parses a query string with the application/x-www-form-urlencoded
// rules, sorts the pairs by key and renders them again. Sorting always triggers
// a re-serialisation in JavaScript, so the original escaping is not preserved.
func encodeURLQuery(raw string) string {
	pairs := []queryPair{}
	for raw != "" {
		part := raw
		if i := strings.IndexByte(raw, '&'); i >= 0 {
			part, raw = raw[:i], raw[i+1:]
		} else {
			raw = ""
		}
		if part == "" {
			continue
		}
		key, value := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			key, value = part[:i], part[i+1:]
		}
		pairs = append(pairs, queryPair{
			key:   decodeURLEncoded(key),
			value: decodeURLEncoded(value),
		})
	}
	if len(pairs) == 0 {
		return ""
	}
	// URLSearchParams.sort() is stable and compares UTF-16 code units. Every
	// key a share link uses is ASCII, where that equals byte order.
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	var b strings.Builder
	for i, pair := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(encodeURLEncoded(pair.key))
		b.WriteByte('=')
		b.WriteString(encodeURLEncoded(pair.value))
	}
	return b.String()
}

// decodeURLEncoded mirrors URLSearchParams parsing: "+" is a space, "%XX" is a
// byte, and an invalid "%" sequence is kept literally.
func decodeURLEncoded(s string) string {
	if strings.IndexByte(s, '%') < 0 && strings.IndexByte(s, '+') < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]):
			b.WriteByte(hexValue(s[i+1])<<4 | hexValue(s[i+2]))
			i += 2
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// encodeURLEncoded is the application/x-www-form-urlencoded serializer: only
// "*" "-" "." "_" and alphanumerics survive, space becomes "+".
func encodeURLEncoded(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c == '*' || c == '-' || c == '.' || c == '_' ||
			('0' <= c && c <= '9') || ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z'):
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0xf])
		}
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"

func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func hexValue(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// escapeWhatWG percent-encodes every byte outside the safe set, including all
// non-ASCII bytes, which are encoded as their UTF-8 octets.
func escapeWhatWG(value string, safe string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x80 && strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0xf])
	}
	return b.String()
}
