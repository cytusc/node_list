package domain

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ShareLinkVersion bumps whenever the converter changes. Persisted failures
// tagged with an older version re-enter the queue so a fixed parser retries
// nodes that previously looked unsupported.
const ShareLinkVersion = 9

// ErrShareLink is returned for any link this converter cannot express as a
// usable outbound. The message never includes the link itself: share links
// embed credentials and are stored alongside node records.
var ErrShareLink = fmt.Errorf("节点分享链接格式或连接参数不受当前检测服务支持")

// ShareLinkError contains only fixed diagnostic codes, never link values.
type ShareLinkError struct{ Code string }

func (e *ShareLinkError) Error() string { return ErrShareLink.Error() + ": " + e.Code }
func (e *ShareLinkError) Unwrap() error { return ErrShareLink }
func shareLinkError(code string) error  { return &ShareLinkError{Code: code} }

// Mihomo and Xray map custom VLESS IDs to UUIDv5 in the nil namespace.
func normalizeVLESSID(value string) (string, error) {
	if err := checkValues(value); err != nil {
		return "", err
	}
	if value == "" {
		return "", shareLinkError("missing-credentials")
	}
	if id, err := normalizeUUID(value); err == nil {
		return id, nil
	}
	sum := sha1.Sum(append(make([]byte, 16), []byte(value)...))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16]), nil
}

func decodeBase64(value string) (string, error) {
	if b, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return string(b), nil
	}
	if b, err := base64.URLEncoding.DecodeString(value); err == nil {
		return string(b), nil
	}
	if b, err := base64.StdEncoding.DecodeString(value); err == nil {
		return string(b), nil
	}
	b, err := base64.RawStdEncoding.DecodeString(value)
	return string(b), err
}

func validECHConfig(value string) bool {
	decoded, err := decodeBase64(value)
	if err != nil || len(decoded) < 6 {
		return false
	}
	b := []byte(decoded)
	if int(binary.BigEndian.Uint16(b[:2])) != len(b)-2 {
		return false
	}
	for offset := 2; offset < len(b); {
		if len(b)-offset < 4 {
			return false
		}
		entrySize := int(binary.BigEndian.Uint16(b[offset+2 : offset+4]))
		offset += 4
		if entrySize == 0 || entrySize > len(b)-offset {
			return false
		}
		offset += entrySize
	}
	return true
}

// parseECHSetting accepts either an ECHConfigList or the Qilan DNS form
// "query-name+resolver-url". Mihomo resolves ECH HTTPS records through its
// proxy-server resolver, so callers must retain resolver for the generated
// top-level proxy-server-nameserver-policy.
func parseECHSetting(value string) (config, queryServerName, resolver string, ok bool) {
	if validECHConfig(value) {
		return value, "", "", true
	}
	queryServerName, resolver, found := strings.Cut(strings.TrimSpace(value), "+")
	if !found || queryServerName == "" || strings.ContainsAny(queryServerName, "/:@?# \t\r\n") {
		return "", "", "", false
	}
	if net.ParseIP(queryServerName) != nil || !strings.Contains(queryServerName, ".") {
		return "", "", "", false
	}
	u, err := url.Parse(resolver)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", "", "", false
	}
	switch u.Scheme {
	case "udp", "tcp":
		if u.Path != "" || u.RawQuery != "" {
			return "", "", "", false
		}
	case "https":
		if u.RawQuery != "" {
			return "", "", "", false
		}
	default:
		return "", "", "", false
	}
	return "", queryServerName, u.String(), true
}

func flagValue(v string) bool {
	switch v {
	case "true", "1":
		return true
	}
	return false
}

func listValue(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeUUID accepts both the dashed and the 32-hex compact form.
func normalizeUUID(v string) (string, error) {
	if len(v) == 32 && isHex(v) {
		v = fmt.Sprintf("%s-%s-%s-%s-%s", v[0:8], v[8:12], v[12:16], v[16:20], v[20:32])
	}
	if len(v) != 36 || !isHex(strings.ReplaceAll(v, "-", "")) {
		return "", ErrShareLink
	}
	return v, nil
}

// normalizeTUICCredential supports UUID/password v5 and token-only v4 links.
// Bare non-UUID credentials are v4 tokens, not incomplete v5 credentials.
func normalizeTUICCredential(proxy ProxyConfig, user, password string) error {
	id, err := normalizeUUID(user)
	if err == nil {
		proxy["uuid"] = id
		proxy["password"] = password
		return nil
	}
	if password == "" {
		if user == "" {
			return shareLinkError("missing-credentials")
		}
		proxy["token"] = user
		return nil
	}
	proxy["username"] = user
	proxy["password"] = password
	return nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

// ShareLinkProxy converts an archived share link into a Mihomo outbound. The
// original link stays the node's identity; this conversion is local only, so
// credentials never leave the detector.
func ShareLinkProxy(link, name string) (ProxyConfig, error) {
	if link == "" {
		return nil, ErrShareLink
	}
	if name == "" {
		name = "Archived node"
	}
	switch {
	case strings.HasPrefix(link, "vmess://"):
		return vmessProxy(link, name)
	case strings.HasPrefix(link, "ssr://"):
		return ssrProxy(link, name)
	case strings.HasPrefix(link, "ss://"):
		if u, err := url.Parse(link); err == nil && u.User == nil {
			// SIP002 allows a base64 userinfo or a base64 whole endpoint.
			encoded, _, _ := strings.Cut(strings.TrimPrefix(link, "ss://"), "#")
			encoded, _, _ = strings.Cut(encoded, "?")
			decoded, err := decodeBase64(encoded)
			if err != nil {
				return nil, ErrShareLink
			}
			link = "ss://" + decoded
			if u.RawQuery != "" {
				link += "?" + u.RawQuery
			}
			if u.Fragment != "" {
				link += "#" + u.EscapedFragment()
			}
		}
		return urlProxy(link, name)
	default:
		return urlProxy(link, name)
	}
}

func vmessProxy(link, name string) (ProxyConfig, error) {
	raw, err := decodeBase64(strings.TrimPrefix(link, "vmess://"))
	if err != nil {
		return nil, ErrShareLink
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, ErrShareLink
	}
	proxy := ProxyConfig{
		"name":    name,
		"type":    "vmess",
		"server":  str(config["add"]),
		"port":    toInt(config["port"]),
		"udp":     true,
		"alterId": toInt(config["aid"]),
		"cipher":  "auto",
	}
	if v, ok := config["scy"].(string); ok && v != "" {
		proxy["cipher"] = v
	}
	id, err := normalizeUUID(str(config["id"]))
	if err != nil {
		return nil, ErrShareLink
	}
	proxy["uuid"] = id

	get := func(key string) string {
		switch key {
		case "type":
			return str(config["net"])
		case "headerType":
			return str(config["type"])
		case "sni":
			if v := str(config["sni"]); v != "" {
				return v
			}
			return str(config["host"])
		case "path":
			return str(config["path"])
		case "host":
			return str(config["host"])
		case "serviceName":
			return str(config["serviceName"])
		case "fp":
			return str(config["fp"])
		case "alpn":
			return str(config["alpn"])
		case "ech":
			return str(config["ech"])
		case "allowInsecure":
			for _, k := range []string{"allowInsecure", "insecure", "skip-cert-verify"} {
				if v, ok := config[k]; ok && truthy(v) {
					return "true"
				}
			}
			return ""
		}
		return ""
	}
	tlsEnabled := false
	switch t := config["tls"].(type) {
	case string:
		tlsEnabled = t == "tls"
	case bool:
		tlsEnabled = t
	}
	applyTLSSettings(proxy, get, tlsEnabled)
	if err := applyTransport(proxy, get); err != nil {
		return nil, err
	}
	return validateProxy(proxy)
}

func ssrProxy(link, name string) (ProxyConfig, error) {
	raw, err := decodeBase64(strings.TrimPrefix(link, "ssr://"))
	if err != nil {
		return nil, ErrShareLink
	}
	main, query := raw, ""
	if i := strings.Index(main, "/?"); i >= 0 {
		main, query = main[:i], main[i+2:]
	}
	fields := strings.Split(main, ":")
	if len(fields) < 6 {
		return nil, ErrShareLink
	}
	port, protocol, cipher, obfs, password :=
		fields[len(fields)-5], fields[len(fields)-4], fields[len(fields)-3],
		fields[len(fields)-2], fields[len(fields)-1]
	decodedPassword, err := decodeBase64(password)
	if err != nil {
		return nil, ErrShareLink
	}
	proxy := ProxyConfig{
		"name":     name,
		"type":     "ssr",
		"server":   strings.Join(fields[:len(fields)-5], ":"),
		"port":     toInt(port),
		"protocol": protocol,
		"cipher":   cipher,
		"obfs":     obfs,
		"password": decodedPassword,
		"udp":      true,
	}
	params, err := url.ParseQuery(query)
	if err != nil {
		return nil, ErrShareLink
	}
	for input, output := range map[string]string{"obfsparam": "obfs-param", "protoparam": "protocol-param"} {
		if v := params.Get(input); v != "" {
			decoded, err := decodeBase64(v)
			if err != nil {
				return nil, ErrShareLink
			}
			proxy[output] = decoded
		}
	}
	return validateProxy(proxy)
}

// bracketBareIPv6Authority rewrites an authority whose host is an unbracketed
// IPv6 literal into bracketed form. net/url rejects bare multiple-colon hosts
// ("://user:pw@2001:db8::1:443"); how lenient it is about that shape drifts
// across Go releases, so subscription links written without brackets must be
// normalized before parsing. The rewrite only fires when the tail segment is
// a decimal port and the remainder parses as IPv6, or when the whole tail
// parses as a bare IPv6 literal; anything else is returned untouched for
// url.Parse to judge.
func bracketBareIPv6Authority(link string) (string, error) {
	rest, ok := strings.CutPrefix(link, "//")
	if !ok {
		i := strings.Index(link, "://")
		if i < 0 {
			return link, nil
		}
		rest = link[i+3:]
	}
	prefix := link[:len(link)-len(rest)]
	// The authority ends at the first '/', '?' or '#'.
	end := len(rest)
	for _, sep := range []string{"/", "?", "#"} {
		if i := strings.Index(rest, sep); i >= 0 && i < end {
			end = i
		}
	}
	authority := rest[:end]
	at := strings.LastIndex(authority, "@")
	hostPart := authority
	if at >= 0 {
		hostPart = authority[at+1:]
	}
	if hostPart == "" || strings.HasPrefix(hostPart, "[") || !strings.Contains(hostPart, ":") {
		return link, nil
	}
	var host, port string
	if colon := strings.LastIndex(hostPart, ":"); colon >= 0 {
		suffix := hostPart[colon+1:]
		switch {
		case strings.Contains(hostPart[:colon], ":") && (suffix == "" || isAllDigits(suffix)) && net.ParseIP(hostPart[:colon]) != nil:
			// Tail is a port on an IPv6 literal ("2001:db8::1:443").
			host, port = hostPart[:colon], suffix
		case net.ParseIP(hostPart) != nil:
			// The whole remnant is a bare IPv6 literal with no port.
			host = hostPart
		default:
			return link, nil
		}
	} else {
		return link, nil
	}
	newHost := "[" + host + "]"
	if port != "" {
		newHost += ":" + port
	}
	return prefix + authority[:at+1] + newHost + rest[end:], nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func urlProxy(link, name string) (ProxyConfig, error) {
	normalized, err := bracketBareIPv6Authority(strings.ReplaceAll(link, "&amp;", "&"))
	if err != nil {
		return nil, ErrShareLink
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return nil, ErrShareLink
	}
	scheme := strings.ToLower(u.Scheme)
	typeName := scheme
	switch scheme {
	case "hy2":
		typeName = "hysteria2"
	case "socks":
		typeName = "socks5"
	}
	// net/url has already percent-decoded URL userinfo. Decoding it again
	// corrupts credentials containing a literal percent (for example, `%25`)
	// and can reject otherwise valid subscription links.
	user := u.User.Username()
	secret, _ := u.User.Password()
	password := secret
	query := u.Query()
	// ECH uses '+' both in base64 and between its DNS name and resolver.
	// Subscription URLs often leave it literal; form decoding would turn it
	// into a space. Preserve it only for ECH, without changing other fields.
	for _, part := range strings.Split(u.RawQuery, "&") {
		key, value, _ := strings.Cut(part, "=")
		if decoded, err := url.QueryUnescape(key); err == nil && decoded == "ech" {
			if value, err := url.QueryUnescape(strings.ReplaceAll(value, "+", "%2B")); err == nil {
				query.Set("ech", value)
			}
			break
		}
	}
	get := query.Get

	proxy := ProxyConfig{
		"name":   name,
		"type":   typeName,
		"server": strings.Trim(u.Hostname(), "[]"),
		"port":   toInt(u.Port()),
		"udp":    true,
	}
	switch typeName {
	case "vless":
		id, err := normalizeVLESSID(user)
		if err != nil {
			return nil, err
		}
		proxy["uuid"] = id
		if v := get("flow"); v != "" {
			proxy["flow"] = v
		}
		if v := get("encryption"); v != "" {
			proxy["encryption"] = v
		}
		if v := get("packetEncoding"); v != "" {
			proxy["packet-encoding"] = v
		}
		applyTLSSettings(proxy, get, urlTLSEnabled(get))
		if err := applyTransport(proxy, get); err != nil {
			return nil, err
		}
	case "trojan", "anytls", "hysteria2":
		if password != "" {
			proxy["password"] = user + ":" + password
		} else {
			proxy["password"] = user
		}
		if proxy["password"] == "" {
			return nil, shareLinkError("missing-credentials")
		}
		// TLS-only schemes, expressed explicitly: these outbounds fail without
		// TLS, so the link's own transport never downgrades them. A reality
		// security parameter is still validated inside applyTLSSettings.
		applyTLSSettings(proxy, get, true)
		if typeName == "trojan" {
			if err := applyTransport(proxy, get); err != nil {
				return nil, err
			}
		}
		if typeName == "hysteria2" {
			if v := get("obfs"); v != "" {
				proxy["obfs"] = v
			}
			if v := get("obfs-password"); v != "" {
				proxy["obfs-password"] = v
			}
		}
	case "ss":
		auth := ""
		if password != "" {
			auth = user + ":" + password
		} else {
			decoded, err := decodeBase64(user)
			if err != nil {
				return nil, ErrShareLink
			}
			auth = decoded
		}
		sep := strings.Index(auth, ":")
		if sep < 1 {
			return nil, ErrShareLink
		}
		proxy["cipher"] = auth[:sep]
		proxy["password"] = auth[sep+1:]
		if v := get("plugin"); v != "" {
			opts, err := parsePlugin(v)
			if err != nil {
				return nil, err
			}
			proxy["plugin"], proxy["plugin-opts"] = opts.plugin, opts.opts
		}
	case "socks5", "http":
		if user != "" {
			proxy["username"] = user
		}
		if password != "" {
			proxy["password"] = password
		} else if user != "" {
			if decoded, err := decodeBase64(user); err == nil {
				if sep := strings.Index(decoded, ":"); sep >= 0 {
					proxy["username"] = decoded[:sep]
					proxy["password"] = decoded[sep+1:]
				}
			}
		}
		if u.Port() == "" {
			proxy["port"] = 0
		}
	case "https":
		proxy["type"] = "http"
		applyTLSSettings(proxy, get, true)
		if user != "" {
			proxy["username"] = user
		}
		if password != "" {
			proxy["password"] = password
		}
		if u.Port() == "" {
			proxy["port"] = 443
		}
	case "tuic":
		// TUIC supports v4 token-only links and v5 UUID/password links.
		// Normalize credentials before applying their shared TLS options.
		if err := normalizeTUICCredential(proxy, user, password); err != nil {
			return nil, err
		}
		// tuic v5 always dials over TLS (QUIC); the parameter is unconditional
		// on the scheme, not on stray query keys.
		applyTLSSettings(proxy, get, true)
		if u.Port() == "" {
			proxy["port"] = 443
		}
		for _, key := range []string{"congestion-controller", "udp-relay-mode"} {
			if v := get(key); v != "" {
				proxy[key] = v
			}
		}
	default:
		return nil, ErrShareLink
	}
	return validateProxy(proxy)
}

type pluginResult struct {
	plugin string
	opts   map[string]any
}

func parsePlugin(v string) (pluginResult, error) {
	parts := strings.Split(v, ";")
	values := make(map[string]any)
	for _, option := range parts[1:] {
		if option == "" {
			continue
		}
		if i := strings.Index(option, "="); i >= 0 {
			values[option[:i]] = option[i+1:]
		} else {
			values[option] = true
		}
	}
	switch parts[0] {
	case "obfs-local", "simple-obfs":
		return pluginResult{"obfs", map[string]any{
			"mode": values["obfs"],
			"host": values["obfs-host"],
		}}, nil
	case "v2ray-plugin":
		if values["mode"] == nil {
			values["mode"] = "websocket"
		}
		if values["mode"] != "websocket" {
			return pluginResult{}, shareLinkError("unsupported-plugin-option")
		}
		return pluginResult{"v2ray-plugin", values}, nil
	}
	return pluginResult{}, shareLinkError("unsupported-plugin")
}

// applyTransport converts the link's transport parameters into the Mihomo
// outbound options. Unknown networks are rejected instead of silently dialling
// with the wrong transport.
func applyTransport(proxy ProxyConfig, get func(string) string) error {
	network := get("type")
	// Some Qilan links append the display tag to a URL-escaped transport value
	// (for example, type=tcp%23tag). It is URI fragment metadata, not a network.
	if prefix, _, found := strings.Cut(network, "#"); found {
		network = strings.TrimSpace(prefix)
	}
	if network == "" {
		network = "tcp"
	}
	if network == "splithttp" {
		network = "xhttp"
	}
	if network == "raw" {
		network = "tcp"
	}
	switch network {
	case "tcp", "ws", "grpc", "http", "h2", "httpupgrade", "xhttp":
	default:
		return shareLinkError("unsupported-transport")
	}
	host := get("host")
	if host == "" {
		host = get("authority")
	}
	path := get("path")
	if path == "" {
		path = "/"
	}
	if network == "tcp" && get("headerType") == "http" {
		network = "http"
	}
	switch {
	case network == "ws" || network == "httpupgrade":
		proxy["network"] = "ws"
		opts := map[string]any{"path": path}
		if host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		if network == "httpupgrade" {
			opts["v2ray-http-upgrade"] = true
		}
		if v := first(get, "ed", "max_early_data"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				opts["max-early-data"] = n
			}
		}
		if v := first(get, "eh", "early_data_header_name"); v != "" {
			opts["early-data-header-name"] = v
		}
		proxy["ws-opts"] = opts
	case network == "grpc":
		proxy["network"] = network
		proxy["grpc-opts"] = map[string]any{
			"grpc-service-name": firstOr(get, "serviceName", "path"),
		}
	case network == "http":
		proxy["network"] = network
		opts := map[string]any{"path": []string{path}}
		if host != "" {
			opts["headers"] = map[string]any{"Host": listValue(host)}
		}
		proxy["http-opts"] = opts
	case network == "h2":
		proxy["network"] = network
		opts := map[string]any{"path": path}
		if host != "" {
			opts["host"] = listValue(host)
		}
		proxy["h2-opts"] = opts
	case network == "xhttp":
		if proxy["type"] != "vless" {
			return ErrShareLink
		}
		proxy["network"] = network
		opts := map[string]any{"path": path}
		if host != "" {
			opts["host"] = host
		}
		if v := get("mode"); v != "" {
			opts["mode"] = v
		}
		if v := get("extra"); v != "" {
			var extra map[string]any
			if err := json.Unmarshal([]byte(v), &extra); err != nil {
				return shareLinkError("invalid-xhttp-extra")
			}
			aliases := map[string]string{
				"mode":                 "mode",
				"noGRPCHeader":         "no-grpc-header",
				"xPaddingBytes":        "x-padding-bytes",
				"scMaxEachPostBytes":   "sc-max-each-post-bytes",
				"scMinPostsIntervalMs": "sc-min-posts-interval-ms",
				"sessionIDKey":         "session-key",
				"sessionIDPlacement":   "session-placement",
			}
			for key, value := range extra {
				if key == "xmux" {
					reuse, err := xhttpReuseSettings(value)
					if err != nil {
						return err
					}
					opts["reuse-settings"] = reuse
					continue
				}
				if key == "scMaxConcurrentPosts" {
					// Obsolete client scheduling hint, absent from both current
					// Xray and Mihomo. It has no wire-format counterpart.
					if _, err := xhttpRange(value); err != nil {
						return err
					}
					continue
				}
				if key == "headers" {
					opts["headers"] = value
					continue
				}
				target, ok := aliases[key]
				if !ok {
					return shareLinkError("unsupported-xhttp-option")
				}
				opts[target] = value
			}
		}
		proxy["xhttp-opts"] = opts
	}
	return nil
}

func xhttpRange(value any) (string, error) {
	switch v := value.(type) {
	case string:
		parts := strings.Split(v, "-")
		if len(parts) > 2 {
			break
		}
		previous := int64(0)
		for _, part := range parts {
			n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil || n < previous {
				return "", shareLinkError("invalid-xhttp-extra")
			}
			previous = n
		}
		return v, nil
	case float64:
		if v >= 0 && v < math.MaxInt64 && v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10), nil
		}
	}
	return "", shareLinkError("invalid-xhttp-extra")
}

func xhttpReuseSettings(value any) (map[string]any, error) {
	values, ok := value.(map[string]any)
	if !ok {
		return nil, shareLinkError("invalid-xhttp-extra")
	}
	aliases := map[string]string{
		"maxConcurrency": "max-concurrency", "maxConnections": "max-connections",
		"cMaxReuseTimes": "c-max-reuse-times", "hMaxRequestTimes": "h-max-request-times",
		"hMaxReusableSecs": "h-max-reusable-secs", "hKeepAlivePeriod": "h-keep-alive-period",
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		target, ok := aliases[key]
		if !ok {
			return nil, shareLinkError("unsupported-xhttp-option")
		}
		v, err := xhttpRange(value)
		if err != nil {
			return nil, err
		}
		if key == "hKeepAlivePeriod" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, shareLinkError("invalid-xhttp-extra")
			}
			out[target] = n
		} else {
			out[target] = v
		}
	}
	return out, nil
}

func first(get func(string) string, keys ...string) string {
	for _, k := range keys {
		if v := get(k); v != "" {
			return v
		}
	}
	return ""
}

func firstOr(get func(string) string, keys ...string) string {
	return first(get, keys...)
}

// urlTLSEnabled derives TLS from the link's own security parameter. Trojan,
// anytls, hysteria2 and tuic are TLS-only by scheme, so their callers pass
// true unconditionally; only vmess (JSON tls field) and vless (security
// query) need derivation.
func urlTLSEnabled(get func(string) string) bool {
	security := get("security")
	return security == "tls" || security == "reality"
}

func applyTLSSettings(proxy ProxyConfig, get func(string) string, enabled bool) {
	if enabled {
		proxy["tls"] = true
	}
	if sni := first(get, "sni", "serverName"); sni != "" {
		if proxy["type"] == "vmess" || proxy["type"] == "vless" {
			proxy["servername"] = sni
		} else {
			proxy["sni"] = sni
		}
	}
	if v := get("fp"); v != "" {
		proxy["client-fingerprint"] = v
	}
	if v := get("alpn"); v != "" {
		proxy["alpn"] = listValue(v)
	}
	if v := first(get, "allowInsecure", "allowinsecure", "insecure"); flagValue(v) {
		proxy["skip-cert-verify"] = true
	}
	if v := get("pinSHA256"); v != "" {
		proxy["fingerprint"] = v
	}
	if v := get("ech"); v != "" {
		config, queryServerName, resolver, ok := parseECHSetting(v)
		if !ok {
			proxy["__bad-ech"] = true
		} else if config != "" {
			proxy["ech-opts"] = map[string]any{"enable": true, "config": config}
		} else {
			proxy["ech-opts"] = map[string]any{"enable": true, "query-server-name": queryServerName}
			proxy["__ech-resolver"] = resolver
		}
	}
	if get("security") == "reality" {
		publicKey := first(get, "pbk", "publicKey", "public-key", "public_key")
		if publicKey == "" {
			proxy["__bad-reality"] = true
			return
		}
		proxy["reality-opts"] = map[string]any{
			"public-key": publicKey,
			"short-id":   get("sid"),
		}
	}
}

// validateProxy rejects configurations that cannot be dialled and any value
// carrying control characters, which would corrupt a Mihomo YAML document.
func validateProxy(proxy ProxyConfig) (ProxyConfig, error) {
	if proxy["__bad-reality"] != nil {
		return nil, shareLinkError("missing-reality-key")
	}
	if proxy["__bad-ech"] != nil {
		return nil, shareLinkError("invalid-ech")
	}
	server, _ := proxy["server"].(string)
	port, _ := proxy["port"].(int)
	if server == "" || port < 1 || port > 65535 {
		return nil, shareLinkError("invalid-endpoint")
	}
	if err := checkValues(map[string]any(proxy)); err != nil {
		return nil, err
	}
	return proxy, nil
}

func checkValues(v any) error {
	switch t := v.(type) {
	case string:
		if !utf8.ValidString(t) {
			return shareLinkError("invalid-text")
		}
		for _, c := range t {
			if unicode.IsControl(c) {
				return shareLinkError("control-character")
			}
		}
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return ErrShareLink
		}
	case int:
	case bool:
	case nil:
	case map[string]any:
		for _, value := range t {
			if err := checkValues(value); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range t {
			if err := checkValues(value); err != nil {
				return err
			}
		}
	case []string:
		for _, value := range t {
			if err := checkValues(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return flagValue(t)
	case float64:
		return t != 0
	}
	return false
}

func toInt(v any) int {
	switch t := v.(type) {
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}
