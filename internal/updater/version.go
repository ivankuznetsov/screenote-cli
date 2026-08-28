package updater

import (
	"strconv"
	"strings"
)

type stableVersion [3]uint64

func parseStableVersion(value string) (stableVersion, bool) {
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return stableVersion{}, false
	}

	var parsed stableVersion
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return stableVersion{}, false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return stableVersion{}, false
			}
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return stableVersion{}, false
		}
		parsed[index] = number
	}
	return parsed, true
}

func normalizeStableVersion(value string) (string, bool) {
	parsed, ok := parseStableVersion(value)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(parsed[0], 10) + "." +
		strconv.FormatUint(parsed[1], 10) + "." +
		strconv.FormatUint(parsed[2], 10), true
}

func newerVersion(latest, current string) bool {
	latestVersion, latestOK := parseStableVersion(latest)
	currentVersion, currentOK := parseStableVersion(current)
	if !latestOK || !currentOK {
		return false
	}
	for index := range latestVersion {
		if latestVersion[index] != currentVersion[index] {
			return latestVersion[index] > currentVersion[index]
		}
	}
	return false
}
