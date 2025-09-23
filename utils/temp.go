package utils

import (
	"os"
	"path/filepath"
)

func GetDirTmpExports() string {
	projectDir, _ := os.Getwd()
	tmpDir := filepath.Join(projectDir, "tmp", "exports")
	os.MkdirAll(tmpDir, 0755)
	return tmpDir
}
