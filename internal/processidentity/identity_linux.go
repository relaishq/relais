package processidentity

import (
	"fmt"
	"os"
	"strings"
)

func inspect(pid int) (string, bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false, err
	}
	return parseStat(string(data))
}
func parseStat(stat string) (string, bool, error) {
	// comm can contain spaces and parentheses; fields begin after its last ')'.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", false, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return "", false, fmt.Errorf("short process stat")
	}
	return fields[19], fields[0] == "Z" || fields[0] == "X", nil
}
