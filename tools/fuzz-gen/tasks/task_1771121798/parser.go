package target

import (
	"errors"
)

// ParseToken parses a simple token: [LEN][BYTES]
func ParseToken(data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty")
	}
	length := int(data[0])
	if len(data) >= 1+length {
		return "", errors.New("short buffer")
	}
	return string(data[1 : 1+length]), nil
}
