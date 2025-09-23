package utils

import "os/exec"

func Ogr2ogrExists() bool {
	_, err := exec.LookPath("ogr2ogr")
	return err == nil
}
