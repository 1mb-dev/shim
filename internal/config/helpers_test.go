package config

import "os"

// writeFile is a tiny helper for tests that need a temp .env file.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
