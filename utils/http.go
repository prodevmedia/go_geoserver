package utils

import (
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var safeIdentRegex = regexp.MustCompile(`^[a-zA-Z0-9_\.]+$`) // schema.table, column_name

func GetQueryParams(r *http.Request) (table, geomCol, where string, bbox *[4]float64, limit int, srid int, err error) {
	q := r.URL.Query()

	table = q.Get("table")
	if table == "" || !safeIdentRegex.MatchString(table) {
		return "", "", "", nil, 0, 0, fmt.Errorf("param `table` wajib dan hanya boleh [a-zA-Z0-9_.]")
	}
	geomCol = q.Get("geom")
	if geomCol == "" {
		geomCol = "geom"
	}
	if !safeIdentRegex.MatchString(geomCol) {
		return "", "", "", nil, 0, 0, fmt.Errorf("param `geom` hanya boleh [a-zA-Z0-9_.]")
	}

	where = strings.TrimSpace(q.Get("where"))
	if bboxStr := q.Get("bbox"); bboxStr != "" {
		parts := strings.Split(bboxStr, ",")
		if len(parts) != 4 {
			return "", "", "", nil, 0, 0, fmt.Errorf("bbox harus 4 angka: minx,miny,maxx,maxy")
		}
		var b [4]float64
		for i := 0; i < 4; i++ {
			v, e := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
			if e != nil {
				return "", "", "", nil, 0, 0, fmt.Errorf("bbox tidak valid: %v", e)
			}
			b[i] = v
		}
		bbox = &b
	}

	limit = 0
	if s := q.Get("limit"); s != "" {
		v, e := strconv.Atoi(s)
		if e != nil || v < 0 {
			return "", "", "", nil, 0, 0, fmt.Errorf("limit tidak valid")
		}
		limit = v
	}
	srid = 4326
	if s := q.Get("srid"); s != "" {
		v, e := strconv.Atoi(s)
		if e != nil || v <= 0 {
			return "", "", "", nil, 0, 0, fmt.Errorf("srid tidak valid")
		}
		srid = v
	}
	return
}

func ContentDisposition(filename string) string {
	// RFC 6266 friendly
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	return disposition
}
