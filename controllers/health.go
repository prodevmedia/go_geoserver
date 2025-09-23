package controllers

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

type HealthController struct {
	db *sql.DB
}

func NewHealthController(db *sql.DB) *HealthController {
	return &HealthController{db: db}
}

func (c HealthController) HandleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := c.db.PingContext(ctx); err != nil {
		http.Error(w, "db not ok: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
