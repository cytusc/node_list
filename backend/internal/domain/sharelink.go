package domain

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// ShareLinkVersion bumps whenever the converter changes. Persisted failures
// tagged with an older version re-enter the queue so a fixed parser retries
// nodes that previously looked unsupported.
const ShareLinkVersion = 3

// ErrShareLink is returned for any link this converter cannot express as a
// usable outbound. The message never includes the link itself: share links
// embed credentials and are stored alongside public snapshot data.
var ErrShareLink = fmt.Errorf("节点分享链接格式或连接参数不受当前检测服务支持")

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

func urlProxy(link, name string) (ProxyConfig, error) {
	u, err := url.Parse(strings.ReplaceAll(link, "&amp;", "&"))
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
	user, _ := url.QueryUnescape(u.User.Username())
	secret, _ := u.User.Password()
	password, _ := url.QueryUnescape(secret)
	get := func(key string) string { return u.Query().Get(key) }

	proxy := ProxyConfig{
		"name":   name,
		"type":   typeName,
		"server": strings.Trim(u.Hostname(), "[]"),
		"port":   toInt(u.Port()),
		"udp":    true,
	}
	switch typeName {
	case "vless":
		id, err := normalizeUUID(user)
		if err != nil {
			return nil, ErrShareLink
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
		applyTLSSettings(proxy, get, get("security") == "tls" || get("security") == "reality")
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
			return nil, ErrShareLink
		}
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
		proxy["tls"] = true
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
		id, err := normalizeUUID(user)
		if err != nil {
			return nil, ErrShareLink
		}
		proxy["uuid"] = id
		proxy["password"] = password
		applyTLSSettings(proxy, get, true)
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
			return pluginResult{}, ErrShareLink
		}
		return pluginResult{"v2ray-plugin", values}, nil
	}
	return pluginResult{}, ErrShareLink
}

// applyTransport converts the link's transport parameters into the Mihomo
// outbound options. Unknown networks are rejected instead of silently dialling
// with the wrong transport.
func applyTransport(proxy ProxyConfig, get func(string) string) error {
	network := get("type")
	if network == "" {
		network = "tcp"
	}
	if network == "raw" {
		network = "tcp"
	}
	switch network {
	case "tcp", "ws", "grpc", "http", "h2", "httpupgrade", "xhttp":
	default:
		return ErrShareLink
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
				return ErrShareLink
			}
			aliases := map[string]string{
				"noGRPCHeader":         "no-grpc-header",
				"xPaddingBytes":        "x-padding-bytes",
				"scMaxEachPostBytes":   "sc-max-each-post-bytes",
				"scMinPostsIntervalMs": "sc-min-posts-interval-ms",
			}
			for key, value := range extra {
				if key == "headers" {
					opts["headers"] = value
					continue
				}
				target, ok := aliases[key]
				if !ok {
					return ErrShareLink
				}
				opts[target] = value
			}
		}
		proxy["xhttp-opts"] = opts
	}
	return nil
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
		proxy["__ech"] = true
	}
	if get("security") == "reality" {
		if get("pbk") == "" {
			proxy["__bad-reality"] = true
			return
		}
		proxy["reality-opts"] = map[string]any{
			"public-key": get("pbk"),
			"short-id":   get("sid"),
		}
	}
}

// validateProxy rejects configurations that cannot be dialled and any value
// carrying control characters, which would corrupt a Mihomo YAML document.
func validateProxy(proxy ProxyConfig) (ProxyConfig, error) {
	if proxy["__ech"] != nil || proxy["__bad-reality"] != nil {
		return nil, ErrShareLink
	}
	server, _ := proxy["server"].(string)
	port, _ := proxy["port"].(int)
	if server == "" || port < 1 || port > 65535 {
		return nil, ErrShareLink
	}
	if err := checkValues(proxy); err != nil {
		return nil, err
	}
	return proxy, nil
}

func checkValues(v any) error {
	switch t := v.(type) {
	case string:
		for i := 0; i < len(t); i++ {
			c := t[i]
			if c < 0x20 || (c >= 0x7f && c <= 0x9f) {
				return ErrShareLink
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
