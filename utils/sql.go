package utils

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

func BuildSQL(
	db *sql.DB,
	table, geomCol string,
	where string,
	bbox *[4]float64,
	limit int,
	srid int,
	forCSV bool,
) (string, []any, error) {
	// 1. Ambil kolom non-geometry
	cols, err := getNonGeomColumns(db, table, geomCol)
	if err != nil {
		return "", nil, err
	}

	log.Println("Non-geom cols:", cols)

	// 2. Siapkan filter dinamis
	var args []any
	var filters []string

	if where != "" {
		filters = append(filters, "("+where+")")
	}
	if bbox != nil {
		filters = append(filters,
			fmt.Sprintf("ST_Intersects(%s, ST_MakeEnvelope($1,$2,$3,$4,$5))", pqIdent(geomCol)),
		)
		args = append(args, bbox[0], bbox[1], bbox[2], bbox[3], srid)
	}

	whereSQL := ""
	if len(filters) > 0 {
		whereSQL = "WHERE " + strings.Join(filters, " AND ")
	}

	// 3. Siapkan limit
	lim := ""
	if limit > 0 {
		lim = fmt.Sprintf(" LIMIT %d", limit)
	}

	// 4. Alias biar ga duplikatif
	geom := pqIdent(geomCol)
	tab := pqIdent(table)
	colList := strings.Join(cols, ", ")

	// 5. SQL untuk CSV (pakai WKT)

	if forCSV {
		sql := fmt.Sprintf(`
			SELECT 
				to_jsonb(sub) - '%[1]s' AS props,
				ST_AsText(%[1]s) AS wkt
			FROM (
				SELECT %[2]s, CASE WHEN %[1]s IS NOT NULL AND ST_IsValid(%[1]s) AND NOT ST_IsEmpty(%[1]s) AND NOT ST_AsText(%[1]s) LIKE '%%NaN%%' THEN %[1]s ELSE NULL END AS %[1]s
				FROM %[3]s
				%[4]s
				%[5]s
			) sub
		`, geom, colList, tab, whereSQL, lim)

		return sql, args, nil
	}

	// 6. SQL untuk GeoJSON
	sql := fmt.Sprintf(`
		SELECT 
			ST_AsGeoJSON(ST_Transform(%[1]s, %[2]d)) AS geom_json,
			to_jsonb(sub) AS props
		FROM (
			SELECT %[3]s, CASE WHEN %[1]s IS NOT NULL AND ST_IsValid(%[1]s) AND NOT ST_IsEmpty(%[1]s) AND NOT ST_AsText(%[1]s) LIKE '%%NaN%%' THEN %[1]s ELSE NULL END AS %[1]s
			FROM %[4]s
			%[5]s
			%[6]s
		) sub
	`, geom, srid, colList, tab, whereSQL, lim)

	return sql, args, nil
}

func BuildSQLForOgr(table, geomCol, where string, bbox *[4]float64, limit, srid int) (string, error) {
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

func getNonGeomColumns(db *sql.DB, table, geomCol string) ([]string, error) {
	log.Println("getNonGeomColumns:", table, geomCol)
	query := `
        SELECT column_name
        FROM information_schema.columns
        WHERE table_schema = $1
          AND table_name = $2
          AND column_name <> $3
        ORDER BY ordinal_position
    `
	rows, err := db.Query(query, "public", table, geomCol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, pqIdent(c))
	}
	return cols, nil
}
