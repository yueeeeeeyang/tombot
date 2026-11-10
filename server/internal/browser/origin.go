package browser

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Origin 用 scheme、规范化主机和有效端口绑定许可，不把不同端口、协议或子域视为同一授权目标。
func Origin(address string) (string, error) {
	address, err := NormalizeURL(address)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(address)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if parsed := net.ParseIP(host); parsed != nil {
		host = parsed.String()
	} else {
		host, err = idna.Lookup.ToASCII(host)
		host = strings.TrimSuffix(host, ".")
		if err != nil || host == "" || strings.Contains(host, ":") {
			return "", invalid("Provide a valid web hostname.")
		}
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", invalid("Provide a web port between 1 and 65535.")
		}
		port = strconv.Itoa(number)
	}
	if u.Scheme == "http" && port == "80" || u.Scheme == "https" && port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin := u.Scheme + "://" + host
	if len(origin) > 2048 {
		return "", invalid("The web origin exceeds its length limit.")
	}
	return origin, nil
}
