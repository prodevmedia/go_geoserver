package controllers

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"go_geoserver/database"
	"go_geoserver/utils"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type SyncController struct {
	db *sql.DB
}

func NewSyncController(db *sql.DB) *SyncController {
	return &SyncController{db: db}
}

func (c SyncController) HandleGeoJSON(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := utils.GetQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sql, args, err := utils.BuildSQL(c.db, table, geomCol, where, bbox, limit, srid, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// ctx := r.Context()
	rows, err := c.db.Query(sql, args...)
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

func (c SyncController) HandleCSV(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := utils.GetQueryParams(r)
	_ = srid // tidak dipakai untuk CSV, tapi tetap disahkan agar konsisten
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sql, args, err := utils.BuildSQL(c.db, table, geomCol, where, bbox, limit, srid, true)
	log.Println("SQL for CSV:", sql, args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// ctx := r.Context()
	rows, err := c.db.Query(sql, args...)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer rows.Close()

	filename := fmt.Sprintf("%s.csv", strings.ReplaceAll(strings.ReplaceAll(table, ".", "_"), `"`, ""))
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", utils.ContentDisposition(filename))

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

func (c SyncController) HandleSHP(w http.ResponseWriter, r *http.Request) {
	// SHP diserahkan ke GDAL (ogr2ogr) jika tersedia
	if !utils.Ogr2ogrExists() {
		http.Error(w, "SHP export butuh GDAL/ogr2ogr pada server (tidak ditemukan di PATH)", http.StatusNotImplemented)
		return
	}
	table, geomCol, where, bbox, limit, srid, err := utils.GetQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Bangun SQL final untuk ogr2ogr (tanpa parameter binding, jadi hati2)
	sqlStr, _ := utils.BuildSQLForOgr(table, geomCol, where, bbox, limit, srid)

	// Siapkan direktori sementara untuk files SHP (.shp, .shx, .dbf, .prj)
	tmpDir, err := os.MkdirTemp("", "shp_")
	if err != nil {
		http.Error(w, "temp dir error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)
	base := filepath.Join(tmpDir, "export.shp")

	// Jalankan ogr2ogr
	cmd := exec.Command("ogr2ogr",
		"-f", "ESRI Shapefile",
		base,
		fmt.Sprintf(`PG:%s`, database.DSN_OGR),
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
	if err := utils.ZipDirFiles(tmpDir, zipPath, []string{
		"export.shp", "export.shx", "export.dbf", "export.prj", "export.cpg",
	}); err != nil {
		http.Error(w, "zip error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	filename := strings.ReplaceAll(strings.ReplaceAll(table, ".", "_"), `"`, "") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", utils.ContentDisposition(filename))

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
