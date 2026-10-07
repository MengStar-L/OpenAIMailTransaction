package updater

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\+[0-9A-Za-z.-]+)?$`)

// Versions with a prerelease suffix and development builds never auto-upgrade.
func parseVersion(value string) ([3]uint64, error) {
	var result [3]uint64
	match := stableVersion.FindStringSubmatch(value)
	if match == nil {
		return result, fmt.Errorf("仅正式版本支持安装更新")
	}
	for i := range result {
		n, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return result, fmt.Errorf("版本号无效")
		}
		result[i] = n
	}
	return result, nil
}

func newer(candidate, current string) bool {
	a, errA := parseVersion(candidate)
	b, errB := parseVersion(current)
	if errA != nil || errB != nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func assetVersion(tag string) string { return strings.TrimPrefix(strings.SplitN(tag, "+", 2)[0], "v") }
