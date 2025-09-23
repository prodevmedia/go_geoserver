package main

import (
	"go_geoserver/database"
	"go_geoserver/routes"
	"go_geoserver/utils"
	"log"
	"net/http"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	db := database.InitDB()

	mux := http.NewServeMux()

	routes.API(mux, db)

	handler := utils.LogRequest(utils.WithCORS(mux))

	addr := ":8080"
	log.Printf("Server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}
