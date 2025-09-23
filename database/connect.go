package database

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"
)

var (
	databaseUrlEnv = "DATABASE_URL"
	DSN            string
	DSN_OGR        string
)

func InitDB() *sql.DB {
	DSN = os.Getenv(databaseUrlEnv)
	if DSN == "" {
		log.Fatalf("%s tidak di-set. contoh: postgres://user:pass@host:5432/db?sslmode=disable", databaseUrlEnv)
	}

	u, err := url.Parse(DSN)
	if err != nil {
		panic(err)
	}

	user := u.User.Username()
	pass, _ := u.User.Password()
	host := u.Hostname()
	port := u.Port()
	dbName := u.Path[1:] // strip leading "/"
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}

	// Build clean PG connection string
	DSN_OGR = fmt.Sprintf(
		"host=%s port=%s dbname=%s user=%s password=%s sslmode=%s",
		host, port, dbName, user, pass, sslmode,
	)

	db, err := sql.Open("pgx", DSN)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err = db.Ping(); err != nil {
		log.Fatal(err)
	}
	log.Println("Connected to database")

	return db
}
