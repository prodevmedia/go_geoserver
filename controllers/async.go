package controllers

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"go_geoserver/models"
	"go_geoserver/utils"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	jobs   = map[string]*models.Job{}
	jobsMu sync.Mutex
)

type AsyncController struct {
	db *sql.DB
}

func NewAsyncController(db *sql.DB) *AsyncController {
	return &AsyncController{db: db}
}

// generate unique job_id sederhana
func newJobID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func (c AsyncController) HandleExportAsync(w http.ResponseWriter, r *http.Request) {
	table, geomCol, where, bbox, limit, srid, err := utils.GetQueryParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	jobID := newJobID()
	job := &models.Job{
		ID:        jobID,
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	jobsMu.Lock()
	jobs[jobID] = job
	jobsMu.Unlock()

	// Jalankan di background
	go func(job *models.Job) {
		job.Status = "running"

		sql, args, err := utils.BuildSQL(c.db, table, geomCol, where, bbox, limit, srid, true) // contoh CSV
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
			return
		}

		log.Println("Executing SQL for job", job.ID, ":", sql, args) // Debug log
		// ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		// defer cancel()
		rows, err := c.db.Query(sql, args...)
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
			return
		}
		defer rows.Close()

		// Simpan ke file sementara
		tmpDir := utils.GetDirTmpExports()
		filePath := filepath.Join(tmpDir, fmt.Sprintf("%s.csv", jobID))
		f, err := os.Create(filePath)
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
			return
		}
		defer f.Close()

		cw := csv.NewWriter(f)
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
				_ = json.Unmarshal(propsJSON, &props)
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
				sort.Strings(headers) // supaya urutannya konsisten
				_ = cw.Write(headers)
				headerWritten = true
			}

			row := make([]string, len(headers))
			for i, h := range headers {
				if v, ok := props[h]; ok && v != nil {
					row[i] = fmt.Sprintf("%v", v)
				} else {
					row[i] = "" // <- kosong, bukan <nil>
				}
			}
			_ = cw.Write(row)
		}

		job.Status = "done"
		job.FilePath = filePath
	}(job)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"job_id": jobID,
		"status": "pending",
	})
}

func (c AsyncController) HandleExportStatus(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/async/export/status/")
	jobsMu.Lock()
	log.Println("Checking status for job ID:", jobID) // Debug log
	// all jobs
	log.Println("Current jobs:", jobs) // Debug log
	job, ok := jobs[jobID]
	jobsMu.Unlock()
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(job)
}

func (c AsyncController) HandleDownload(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/async/export/download/")
	jobsMu.Lock()
	job, ok := jobs[jobID]
	jobsMu.Unlock()
	// cek file sudah ada atau status done
	f, err := os.Open(job.FilePath)
	if err != nil {
		if !ok || job.Status != "done" {
			http.Error(w, "job not ready", http.StatusNotFound)
			return
		}
		http.Error(w, "cannot open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", utils.ContentDisposition(filepath.Base(job.FilePath)))
	io.Copy(w, f)
}
