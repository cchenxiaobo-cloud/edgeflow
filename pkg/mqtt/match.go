package mqtt

import "strings"

// MatchTopic reports whether an MQTT topic name matches an MQTT topic filter
// per MQTT-4.7 wildcard rules.
//
// Rules implemented:
//   - The filter and topic are split on '/'; every level must match.
//   - '+' matches exactly one level, including an empty level.
//   - '#' must be the last level of the filter and matches zero or more
//     remaining levels ("sport/#" matches "sport" itself).
//   - Leading/trailing '/' produce empty levels that participate in matching
//     (filter "/finance" matches topic "/finance" but not "finance").
//   - MQTT-4.7.2 (non-normative) convention: topics starting with '$' are not
//     matched by filters starting with '#' or '+'. We generalize this to: a
//     topic whose first level starts with '$' is only matched by filters whose
//     first level also starts with '$'.
func MatchTopic(filter, topic string) bool {
	if filter == "" || topic == "" {
		return filter == "" && topic == ""
	}
	fLevels := strings.Split(filter, "/")
	tLevels := strings.Split(topic, "/")

	// $-topics (e.g. $SYS/...) are only matched by filters whose first level
	// also starts with '$' — wildcard '+'/'#' must not match them.
	if strings.HasPrefix(tLevels[0], "$") && !strings.HasPrefix(fLevels[0], "$") {
		return false
	}

	i := 0
	for ; i < len(fLevels); i++ {
		if fLevels[i] == "#" {
			// '#' must be last; matches zero or more remaining levels.
			return i == len(fLevels)-1
		}
		if i >= len(tLevels) {
			return false
		}
		if fLevels[i] != "+" && fLevels[i] != tLevels[i] {
			return false
		}
	}
	return len(tLevels) == len(fLevels)
}
