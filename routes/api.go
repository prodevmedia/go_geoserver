package routes

import (
	"database/sql"
	"go_geoserver/controllers"
	"net/http"
)

func API(r *http.ServeMux, db *sql.DB) {

	// export async job
	exportController := controllers.NewAsyncController(db)
	r.HandleFunc("/async/export", exportController.HandleExportAsync)
	r.HandleFunc("/async/export/status/{job_id}", exportController.HandleExportStatus)
	r.HandleFunc("/async/export/download/{job_id}", exportController.HandleDownload)

	// sync / direct
	syncController := controllers.NewSyncController(db)
	r.HandleFunc("/geojson", syncController.HandleGeoJSON)
	r.HandleFunc("/csv", syncController.HandleCSV)
	r.HandleFunc("/shp", syncController.HandleSHP)

	// health check
	healthController := controllers.NewHealthController(db)
	r.HandleFunc("/health", healthController.HandleHealth)
}
