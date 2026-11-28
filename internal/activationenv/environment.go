// Package activationenv fixes local activation subprocesses to one direct route.
package activationenv

import "strings"

func Direct(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "APPLESERVICES_NATIVE_HTTP_PROXY", "APPLESERVICES_SOCKS_PROXY", "APPLESERVICES_ACTIVATION_IPV4":
			continue
		}
		result = append(result, entry)
	}
	return append(result, "NO_PROXY=*", "APPLESERVICES_ACTIVATION_IPV4=1")
}
