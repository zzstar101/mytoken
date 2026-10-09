package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataDir returns the platform data directory and creates it with private permissions.
func DataDir() (string, error) {
	dir := os.Getenv("MYTOKEN_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch runtime.GOOS {
		case "darwin":
			dir = filepath.Join(home, "Library", "Application Support", "MyToken")
		case "windows":
			base := os.Getenv("APPDATA")
			if base == "" {
				base = filepath.Join(home, "AppData", "Roaming")
			}
			dir = filepath.Join(base, "MyToken")
		default:
			base := os.Getenv("XDG_DATA_HOME")
			if base == "" {
				base = filepath.Join(home, ".local", "share")
			}
			dir = filepath.Join(base, "mytoken")
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}
func DBPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mytoken.db"), nil
}
