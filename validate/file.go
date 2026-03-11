package validate

import "os"

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
