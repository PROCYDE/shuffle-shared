package shuffle

import (
	"os"
	"strconv"
	"strings"
)

func parseInt(s string) int {
	s = strings.TrimSpace(s)
	val, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return val
}

type StreamFn func(line string)

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func isValidSerial(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))

	if s == "" {
		return false
	}

	bad := []string{
		"to be filled",
		"default string",
		"o.e.m",
		"unknown",
	}

	for _, b := range bad {
		if strings.Contains(s, b) {
			return false
		}
	}

	return true
}
