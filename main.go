package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
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

	// Jika menggunakan Transaction pooler Supabase (port 6543), aktifkan simple_protocol
	// agar tidak terjadi konflik prepared statements dengan PgBouncer/Supavisor
	if strings.Contains(dsn, ":6543") && !strings.Contains(dsn, "default_query_exec_mode") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn = dsn + sep + "default_query_exec_mode=simple_protocol"
	}

	var err error
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(1 * time.Minute)
	db.SetConnMaxLifetime(10 * time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/geojson", handleGeoJSON)
	mux.HandleFunc("/csv", handleCSV)
	mux.HandleFunc("/shp", handleSHP)
	mux.HandleFunc("/kml", handleKML)
	mux.HandleFunc("/gpkg", handleGPKG)

	// GeoServer / OGC WFS compatibility routes
	mux.HandleFunc("/geoserver/", handleWFS)
	mux.HandleFunc("/ows", handleWFS)
	mux.HandleFunc("/wfs", handleWFS)

	// Root & Favicon
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"service":"go_geoserver","status":"ok","version":"1.1.0"}`))
	})

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

	// 1. Ambil nama tabel/layer (dukung format standar GeoServer: typeName, typename, typeNames, layers, layer)
	rawTable := q.Get("table")
	if rawTable == "" {
		rawTable = q.Get("typeName")
	}
	if rawTable == "" {
		rawTable = q.Get("typename")
	}
	if rawTable == "" {
		rawTable = q.Get("typeNames")
	}
	if rawTable == "" {
		rawTable = q.Get("layers")
	}
	if rawTable == "" {
		rawTable = q.Get("layer")
	}
	rawTable = strings.TrimSpace(rawTable)
	if rawTable == "" {
		return "", "", "", nil, 0, 0, fmt.Errorf("param `table` atau `typeName` wajib diisi")
	}

	// Buang prefix workspace GeoServer (contoh: "rehabdas:petaks" -> "petaks")
	if idx := strings.Index(rawTable, ":"); idx != -1 {
		rawTable = rawTable[idx+1:]
	}

	// 2. Ambil WHERE / CQL_FILTER
	where = strings.TrimSpace(q.Get("where"))
	if where == "" {
		where = strings.TrimSpace(q.Get("CQL_FILTER"))
	}
	if where == "" {
		where = strings.TrimSpace(q.Get("cql_filter"))
	}
	if where == "" {
		where = strings.TrimSpace(q.Get("cqlFilter"))
	}

	// 3. Ambil kolom geometri
	geomCol = q.Get("geom")

	// Mapping khusus: jika layer "tanamans" dan filter menggunakan "tanaman_", arahkan ke view v_tanaman_detail
	if (rawTable == "tanamans" || rawTable == "public.tanamans") && (strings.Contains(where, "tanaman_") || geomCol == "tanaman_geom") {
		table = "public.v_tanaman_detail"
		if geomCol == "" || geomCol == "geom" {
			geomCol = "tanaman_geom"
		}
	} else {
		// Pasang default schema public jika belum ada
		if !strings.Contains(rawTable, ".") {
			table = "public." + rawTable
		} else {
			table = rawTable
		}
		if geomCol == "" {
			geomCol = "geom"
		}
	}

	if !safeIdentRegex.MatchString(table) {
		return "", "", "", nil, 0, 0, fmt.Errorf("nama tabel `%s` tidak valid", table)
	}
	if !safeIdentRegex.MatchString(geomCol) {
		return "", "", "", nil, 0, 0, fmt.Errorf("nama kolom geom `%s` tidak valid", geomCol)
	}

	// 4. BBOX
	bboxStr := q.Get("bbox")
	if bboxStr == "" {
		bboxStr = q.Get("BBOX")
	}
	if bboxStr != "" {
		parts := strings.Split(bboxStr, ",")
		if len(parts) >= 4 {
			var b [4]float64
			valid := true
			for i := 0; i < 4; i++ {
				v, e := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
				if e != nil {
					valid = false
					break
				}
				b[i] = v
			}
			if valid {
				bbox = &b
			}
		}
	}

	// 5. LIMIT (dukung maxFeatures, maxfeatures, count)
	limit = 0
	limStr := q.Get("limit")
	if limStr == "" {
		limStr = q.Get("maxFeatures")
	}
	if limStr == "" {
		limStr = q.Get("maxfeatures")
	}
	if limStr == "" {
		limStr = q.Get("count")
	}
	if limStr != "" {
		v, e := strconv.Atoi(limStr)
		// maxFeatures=99999999 dianggap tidak ada limit
		if e == nil && v > 0 && v < 9999999 {
			limit = v
		}
	}

	// 6. SRID (dukung srsName, srs, SRS)
	srid = 4326
	sridStr := q.Get("srid")
	if sridStr == "" {
		sridStr = q.Get("srsName")
	}
	if sridStr == "" {
		sridStr = q.Get("srs")
	}
	if sridStr == "" {
		sridStr = q.Get("SRS")
	}
	if sridStr != "" {
		if idx := strings.LastIndex(sridStr, ":"); idx != -1 {
			sridStr = sridStr[idx+1:]
		}
		v, e := strconv.Atoi(sridStr)
		if e == nil && v > 0 {
			srid = v
		}
	}

	return
}

func buildSQL(table, geomCol, where string, bbox *[4]float64, limit, srid int, forCSV bool) (string, []any) {
	var args []any
	var filters []string
	if where != "" {
		filters = append(filters, "("+where+")")
	}
	if bbox != nil {
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

	// GeoJSON: transform ke SRID target dengan aman
	sql := fmt.Sprintf(`
		SELECT 
			ST_AsGeoJSON(CASE WHEN ST_SRID(%s) = 0 THEN ST_SetSRID(%s, %d) ELSE ST_Transform(%s, %d) END) AS geom_json,
			to_jsonb(t) - '%s' AS props
		FROM %s t
		%s
		%s
	`, pqIdent(geomCol), pqIdent(geomCol), srid, pqIdent(geomCol), srid, geomCol, pqIdent(table), whereSQL, lim)
	return sql, args
}

func streamGeoJSON(ctx context.Context, w io.Writer, table, geomCol, where string, bbox *[4]float64, limit, srid int) error {
	sql, args := buildSQL(table, geomCol, where, bbox, limit, srid, false)

	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("query error: %w", err)
	}
	defer rows.Close()

	if _, err := io.WriteString(w, `{"type":"FeatureCollection","features":[`); err != nil {
		return err
	}

	first := true
	for rows.Next() {
		var geomJSON *string
		var propsJSON []byte
		if err := rows.Scan(&geomJSON, &propsJSON); err != nil {
			log.Println("scan:", err)
			continue
		}
		if geomJSON == nil {
			continue
		}
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false

		if _, err := io.WriteString(w, `{"type":"Feature","geometry":`); err != nil {
			return err
		}
		if _, err := w.Write([]byte(*geomJSON)); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `,"properties":`); err != nil {
			return err
		}
		if len(propsJSON) == 0 {
			if _, err := io.WriteString(w, `{}`); err != nil {
				return err
			}
		} else {
			if _, err := w.Write(propsJSON); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, `}`); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "]}")
	return err
}

func handleGeoJSON(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	layerName := cleanLayerName(table)
	w.Header().Set("Content-Type", "application/geo+json; charset=utf-8")
	w.Header().Set("Content-Disposition", contentDisposition(layerName+".geojson"))
	w.WriteHeader(http.StatusOK)

	if err := streamGeoJSON(r.Context(), w, table, geomCol, where, bbox, limit, srid); err != nil {
		log.Println("stream geojson:", err)
	}
}

func handleCSV(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	_ = srid
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

	layerName := cleanLayerName(table)
	filename := fmt.Sprintf("%s.csv", layerName)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", contentDisposition(filename))

	cw := csv.NewWriter(w)
	defer cw.Flush()

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

		if wkt != nil {
			props["_wkt"] = *wkt
		} else {
			props["_wkt"] = ""
		}

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
	if !ogr2ogrExists() {
		http.Error(w, "SHP export butuh GDAL/ogr2ogr pada server (tidak ditemukan di PATH)", http.StatusNotImplemented)
		return
	}
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	layerName := cleanLayerName(table)

	tmpDir, err := os.MkdirTemp("", "shp_")
	if err != nil {
		http.Error(w, "temp dir error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	shpPath := filepath.Join(tmpDir, layerName+".shp")

	cmd := exec.Command("ogr2ogr",
		"-f", "ESRI Shapefile",
		shpPath,
		"/vsistdin/",
		"-nln", layerName,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "pipe error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		http.Error(w, "ogr2ogr start error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	errChan := make(chan error, 1)
	go func() {
		defer stdin.Close()
		errChan <- streamGeoJSON(r.Context(), stdin, table, geomCol, where, bbox, limit, srid)
	}()

	cmdErr := cmd.Wait()
	streamErr := <-errChan

	if streamErr != nil {
		log.Printf("stream error: %v", streamErr)
		http.Error(w, "database query error: "+streamErr.Error(), http.StatusInternalServerError)
		return
	}

	if cmdErr != nil {
		log.Printf("ogr2ogr error: %v, stderr: %s", cmdErr, stderr.String())
		http.Error(w, "ogr2ogr error: "+stderr.String(), http.StatusInternalServerError)
		return
	}

	zipPath := filepath.Join(tmpDir, layerName+".zip")
	if err := zipDir(tmpDir, zipPath); err != nil {
		http.Error(w, "zip error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	filename := layerName + ".zip"
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

func handleKML(w http.ResponseWriter, r *http.Request) {
	if !ogr2ogrExists() {
		http.Error(w, "KML export butuh GDAL/ogr2ogr pada server (tidak ditemukan di PATH)", http.StatusNotImplemented)
		return
	}
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	layerName := cleanLayerName(table)

	tmpDir, err := os.MkdirTemp("", "kml_")
	if err != nil {
		http.Error(w, "temp dir error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	kmlPath := filepath.Join(tmpDir, layerName+".kml")

	cmd := exec.Command("ogr2ogr",
		"-f", "KML",
		kmlPath,
		"/vsistdin/",
		"-nln", layerName,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "pipe error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		http.Error(w, "ogr2ogr start error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	errChan := make(chan error, 1)
	go func() {
		defer stdin.Close()
		errChan <- streamGeoJSON(r.Context(), stdin, table, geomCol, where, bbox, limit, srid)
	}()

	cmdErr := cmd.Wait()
	streamErr := <-errChan

	if streamErr != nil {
		log.Printf("stream error: %v", streamErr)
		http.Error(w, "database query error: "+streamErr.Error(), http.StatusInternalServerError)
		return
	}

	if cmdErr != nil {
		log.Printf("ogr2ogr error: %v, stderr: %s", cmdErr, stderr.String())
		http.Error(w, "ogr2ogr error: "+stderr.String(), http.StatusInternalServerError)
		return
	}

	filename := layerName + ".kml"
	w.Header().Set("Content-Type", "application/vnd.google-earth.kml+xml")
	w.Header().Set("Content-Disposition", contentDisposition(filename))

	f, err := os.Open(kmlPath)
	if err != nil {
		http.Error(w, "open kml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		log.Println("stream kml:", err)
	}
}

func handleGPKG(w http.ResponseWriter, r *http.Request) {
	if !ogr2ogrExists() {
		http.Error(w, "GPKG export butuh GDAL/ogr2ogr pada server (tidak ditemukan di PATH)", http.StatusNotImplemented)
		return
	}
	table, geomCol, where, bbox, limit, srid, err := getQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	layerName := cleanLayerName(table)

	tmpDir, err := os.MkdirTemp("", "gpkg_")
	if err != nil {
		http.Error(w, "temp dir error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	gpkgPath := filepath.Join(tmpDir, layerName+".gpkg")

	cmd := exec.Command("ogr2ogr",
		"-f", "GPKG",
		gpkgPath,
		"/vsistdin/",
		"-nln", layerName,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "pipe error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		http.Error(w, "ogr2ogr start error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	errChan := make(chan error, 1)
	go func() {
		defer stdin.Close()
		errChan <- streamGeoJSON(r.Context(), stdin, table, geomCol, where, bbox, limit, srid)
	}()

	cmdErr := cmd.Wait()
	streamErr := <-errChan

	if streamErr != nil {
		log.Printf("stream error: %v", streamErr)
		http.Error(w, "database query error: "+streamErr.Error(), http.StatusInternalServerError)
		return
	}

	if cmdErr != nil {
		log.Printf("ogr2ogr error: %v, stderr: %s", cmdErr, stderr.String())
		http.Error(w, "ogr2ogr error: "+stderr.String(), http.StatusInternalServerError)
		return
	}

	filename := layerName + ".gpkg"
	w.Header().Set("Content-Type", "application/x-gpkg")
	w.Header().Set("Content-Disposition", contentDisposition(filename))

	f, err := os.Open(gpkgPath)
	if err != nil {
		http.Error(w, "open gpkg: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		log.Println("stream gpkg:", err)
	}
}

func handleWFS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	reqType := strings.ToLower(q.Get("request"))

	if reqType == "getcapabilities" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<WFS_Capabilities version="1.0.0" xmlns="http://www.opengis.net/wfs" xmlns:ogc="http://www.opengis.net/ogc">
  <Service>
    <Name>WFS</Name>
    <Title>Go GeoServer WFS</Title>
  </Service>
</WFS_Capabilities>`))
		return
	}

	outputFormat := strings.ToLower(q.Get("outputFormat"))
	if outputFormat == "" {
		outputFormat = strings.ToLower(q.Get("outputformat"))
	}
	if outputFormat == "" {
		outputFormat = strings.ToLower(q.Get("format"))
	}

	switch {
	case strings.Contains(outputFormat, "shape") || strings.Contains(outputFormat, "zip") || strings.Contains(outputFormat, "shp"):
		handleSHP(w, r)
	case strings.Contains(outputFormat, "kml"):
		handleKML(w, r)
	case strings.Contains(outputFormat, "gpkg") || strings.Contains(outputFormat, "geopackage"):
		handleGPKG(w, r)
	case strings.Contains(outputFormat, "csv"):
		handleCSV(w, r)
	default:
		// Default to GeoJSON for application/json or GetFeature standard
		handleGeoJSON(w, r)
	}
}

func ogr2ogrExists() bool {
	_, err := exec.LookPath("ogr2ogr")
	return err == nil
}

func cleanLayerName(table string) string {
	parts := strings.Split(table, ".")
	name := parts[len(parts)-1]
	name = strings.ReplaceAll(name, `"`, "")
	if name == "" {
		return "export"
	}
	return name
}

func pqIdent(ident string) string {
	parts := strings.Split(ident, ".")
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, `"`+strings.ReplaceAll(p, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, ".")
}

func contentDisposition(filename string) string {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	return disposition
}

func zipDir(dir, zipPath string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == filepath.Base(zipPath) {
			continue
		}
		full := filepath.Join(dir, name)
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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, HEAD")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
