package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	db             *sql.DB
	connStringEnv  = "DATABASE_URL"
	safeIdentRegex = regexp.MustCompile(`^[a-zA-Z0-9_\.]+$`) // schema.table, column_name
)

func main() {
	dsn := os.Getenv(connStringEnv)
	if dsn == "" {
		log.Fatalf("%s tidak di-set. contoh: postgres://user:pass@host:5432/db?sslmode=disable", connStringEnv)
	}
	var err error
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/geojson", handleGeoJSON)
	mux.HandleFunc("/csv", handleCSV)
	mux.HandleFunc("/shp", handleSHP) // pakai GDAL/ogr2ogr jika tersedia

	handler := logRequest(withCORS(mux))

	addr := ":8080"
	log.Printf("Server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.String(), time.Since(start))
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		http.Error(w, "db not ok: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func getQueryParams(r *http.Request) (table, geomCol, where string, bbox *[4]float64, limit int, srid int, err error) {
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

func buildSQL(table, geomCol, where string, bbox *[4]float64, limit, srid int, forCSV bool) (string, []any) {
	var args []any
	// Bangun klausul WHERE aman-aman saja (where mentah + bbox)
	var filters []string
	if where != "" {
		// PERINGATAN: where diteruskan apa adanya — batasi di reverse proxy/whitelist untuk produksi
		filters = append(filters, "("+where+")")
	}
	if bbox != nil {
		// ST_MakeEnvelope(minx, miny, maxx, maxy, srid)
		filters = append(filters, fmt.Sprintf("ST_Intersects(%s, ST_MakeEnvelope($1,$2,$3,$4,$5))", pqIdent(geomCol)))
		args = append(args, bbox[0], bbox[1], bbox[2], bbox[3], srid)
	}
	whereSQL := ""
	if len(filters) > 0 {
		whereSQL = "WHERE " + strings.Join(filters, " AND ")
	}

	lim := ""
	if limit > 0 {
		lim = fmt.Sprintf(" LIMIT %d", limit)
	}

	if forCSV {
		// Buat semua kolom kecuali geometry dalam SELECT, geometry dihilangkan.
		// Trik: to_jsonb(t) - 'geom' —> dapat properties sebagai jsonb; lalu kita pecah di Go untuk CSV headers.
		// Untuk CSV, ambil props & geom WKT tipis-tipis (opsional).
		sql := fmt.Sprintf(`
			SELECT 
				to_jsonb(t) - '%s' AS props,
				ST_AsText(%s) AS wkt
			FROM %s t
			%s
			%s
		`, geomCol, pqIdent(geomCol), pqIdent(table), whereSQL, lim)
		return sql, args
	}

	// GeoJSON: stream FeatureCollection
	sql := fmt.Sprintf(`
		SELECT 
			ST_AsGeoJSON(ST_Transform(%s, %d)) AS geom_json,
			to_jsonb(t) - '%s' AS props
		FROM %s t
		%s
		%s
	`, pqIdent(geomCol), srid, geomCol, pqIdent(table), whereSQL, lim)
	return sql, args
}

func handleGeoJSON(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sql, args := buildSQL(table, geomCol, where, bbox, limit, srid, false)

	ctx := r.Context()
	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "application/geo+json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `{"type":"FeatureCollection","features":[`)

	first := true
	for rows.Next() {
		var geomJSON *string
		var propsJSON []byte
		if err := rows.Scan(&geomJSON, &propsJSON); err != nil {
			log.Println("scan:", err)
			continue
		}
		if geomJSON == nil {
			// skip baris tanpa geom
			continue
		}
		if !first {
			io.WriteString(w, ",")
		}
		first = false

		// Tulis Feature secara streaming
		w.Write([]byte(`{"type":"Feature","geometry":`))
		w.Write([]byte(*geomJSON))
		w.Write([]byte(`,"properties":`))
		if len(propsJSON) == 0 {
			w.Write([]byte(`{}`))
		} else {
			w.Write(propsJSON)
		}
		w.Write([]byte(`}`))
	}
	io.WriteString(w, "]}")
}

func handleCSV(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	_ = srid // tidak dipakai untuk CSV, tapi tetap disahkan agar konsisten
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sql, args := buildSQL(table, geomCol, where, bbox, limit, srid, true)

	ctx := r.Context()
	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer rows.Close()

	filename := fmt.Sprintf("%s.csv", strings.ReplaceAll(strings.ReplaceAll(table, ".", "_"), `"`, ""))
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", contentDisposition(filename))

	cw := csv.NewWriter(w)
	defer cw.Flush()

	// Header dinamis dari props JSON
	var headerWritten bool
	var headers []string

	for rows.Next() {
		var propsJSON []byte
		var wkt *string
		if err := rows.Scan(&propsJSON, &wkt); err != nil {
			log.Println("scan:", err)
			continue
		}
		var props map[string]any
		if len(propsJSON) > 0 {
			if err := json.Unmarshal(propsJSON, &props); err != nil {
				log.Println("json:", err)
				continue
			}
		} else {
			props = map[string]any{}
		}

		// Tambahkan kolom WKT di akhir
		if wkt != nil {
			props["_wkt"] = wkt
		} else {
			props["_wkt"] = ""
		}

		// Tulis header sekali
		if !headerWritten {
			for k := range props {
				headers = append(headers, k)
			}
			if err := cw.Write(headers); err != nil {
				log.Println("csv header:", err)
				break
			}
			headerWritten = true
		}

		// Urutkan sesuai headers
		row := make([]string, len(headers))
		for i, h := range headers {
			if v, ok := props[h]; ok && v != nil {
				row[i] = fmt.Sprintf("%v", v)
			} else {
				row[i] = ""
			}
		}
		if err := cw.Write(row); err != nil {
			log.Println("csv row:", err)
			break
		}
	}
}

func handleSHP(w http.ResponseWriter, r *http.Request) {
	// SHP diserahkan ke GDAL (ogr2ogr) jika tersedia
	if !ogr2ogrExists() {
		http.Error(w, "SHP export butuh GDAL/ogr2ogr pada server (tidak ditemukan di PATH)", http.StatusNotImplemented)
		return
	}
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Bangun SQL final untuk ogr2ogr (tanpa parameter binding, jadi hati2)
	sqlStr, _ := buildSQLForOgr(table, geomCol, where, bbox, limit, srid)

	// Siapkan direktori sementara untuk files SHP (.shp, .shx, .dbf, .prj)
	tmpDir, err := os.MkdirTemp("", "shp_")
	if err != nil {
		http.Error(w, "temp dir error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)
	base := filepath.Join(tmpDir, "export.shp")

	// Jalankan ogr2ogr
	conn := os.Getenv(connStringEnv)
	cmd := exec.Command("ogr2ogr",
		"-f", "ESRI Shapefile",
		base,
		fmt.Sprintf(`PG:%s`, conn),
		"-dialect", "SQLite", // aman-aman saja; mempermudah limit
		"-sql", sqlStr,
	)
	// Timeout simpel
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, "ogr2ogr error: "+stderr.String(), http.StatusInternalServerError)
		return
	}

	// Zip-kan semua file SHP terkait
	zipPath := filepath.Join(tmpDir, "export.zip")
	if err := zipDirFiles(tmpDir, zipPath, []string{
		"export.shp", "export.shx", "export.dbf", "export.prj", "export.cpg",
	}); err != nil {
		http.Error(w, "zip error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	filename := strings.ReplaceAll(strings.ReplaceAll(table, ".", "_"), `"`, "") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(filename))

	f, err := os.Open(zipPath)
	if err != nil {
		http.Error(w, "open zip: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		log.Println("stream zip:", err)
	}
}

func ogr2ogrExists() bool {
	_, err := exec.LookPath("ogr2ogr")
	return err == nil
}

func buildSQLForOgr(table, geomCol, where string, bbox *[4]float64, limit, srid int) (string, error) {
	if !safeIdentRegex.MatchString(table) || !safeIdentRegex.MatchString(geomCol) {
		return "", errors.New("ident tidak valid")
	}
	var filters []string
	if where != "" {
		filters = append(filters, "("+where+")")
	}
	if bbox != nil {
		filters = append(filters, fmt.Sprintf("ST_Intersects(%s, ST_MakeEnvelope(%f,%f,%f,%f,%d))",
			pqIdent(geomCol), bbox[0], bbox[1], bbox[2], bbox[3], srid))
	}
	whereSQL := ""
	if len(filters) > 0 {
		whereSQL = " WHERE " + strings.Join(filters, " AND ")
	}
	lim := ""
	if limit > 0 {
		// dengan dialect SQLite, LIMIT bisa dipakai langsung
		lim = fmt.Sprintf(" LIMIT %d", limit)
	}
	// Transformasikan ke SRID target agar sesuai .prj
	sqlStr := fmt.Sprintf(`SELECT (ST_Transform(%s, %d)) AS %s, * FROM %s%s%s`,
		pqIdent(geomCol), srid, geomCol, pqIdent(table), whereSQL, lim)
	return sqlStr, nil
}

func pqIdent(ident string) string {
	// sederhana: pecah schema.table → "schema"."table"
	parts := strings.Split(ident, ".")
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, `"`+strings.ReplaceAll(p, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, ".")
}

func contentDisposition(filename string) string {
	// RFC 6266 friendly
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	return disposition
}

func zipDirFiles(dir, zipPath string, names []string) error {
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

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
