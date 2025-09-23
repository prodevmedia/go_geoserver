package utils

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
)

func ZipDirFiles(dir, zipPath string, names []string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()

	// hanya file yang ada yang dimasukkan
	for _, name := range names {
		full := filepath.Join(dir, name)
		if _, err := os.Stat(full); err != nil {
			continue
		}
		if err := addFileToZip(zw, full, name); err != nil {
			return err
		}
	}
	return nil
}

func addFileToZip(zw *zip.Writer, path, name string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, fh)
	return err
}
