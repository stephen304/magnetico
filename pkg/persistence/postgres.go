package persistence

import (
	"database/sql"
	"fmt"
	"net/url"
	"text/template"
	"time"
	"unicode/utf8"

	_ "github.com/jackc/pgx/v4"
	_ "github.com/jackc/pgx/v4/stdlib"

	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type postgresDatabase struct {
	conn      *sql.DB
	schema    string
	origin_id int
}

func makePostgresDatabase(origin_id int, url_ *url.URL) (Database, error) {
	db := new(postgresDatabase)

	db.origin_id = origin_id

	schema := url_.Query().Get("schema")
	if schema == "" {
		db.schema = "magneticod"
		url_.Query().Set("search_path", "magneticod")
	} else {
		db.schema = schema
		url_.Query().Set("search_path", schema)
	}
	url_.Query().Del("schema")

	var err error
	db.conn, err = sql.Open("pgx", url_.String())
	if err != nil {
		return nil, errors.Wrap(err, "sql.Open")
	}

	// > Open may just validate its arguments without creating a connection to the database. To
	// > verify that the data source Name is valid, call Ping.
	// https://golang.org/pkg/database/sql/#Open
	if err = db.conn.Ping(); err != nil {
		return nil, errors.Wrap(err, "sql.DB.Ping")
	}

	// https://github.com/mattn/go-sqlite3/issues/618
	db.conn.SetConnMaxLifetime(0) // https://golang.org/pkg/database/sql/#DB.SetConnMaxLifetime
	db.conn.SetMaxOpenConns(3)
	db.conn.SetMaxIdleConns(3)

	if err := db.setupDatabase(); err != nil {
		return nil, errors.Wrap(err, "setupDatabase")
	}

	return db, nil
}

func (db *postgresDatabase) Engine() databaseEngine {
	return Postgres
}

func (db *postgresDatabase) DoesTorrentExist(infoHash []byte) (bool, error) {
	rows, err := db.conn.Query("SELECT 1 FROM torrents WHERE info_hash = $1;", infoHash)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	exists := rows.Next()
	if rows.Err() != nil {
		return false, err
	}

	return exists, nil
}

func (db *postgresDatabase) AddNewTorrent(infoHash []byte, name string, files []File) error {
	if !utf8.ValidString(name) {
		zap.L().Warn(
			"Ignoring a torrent whose name is not UTF-8 compliant.",
			zap.ByteString("infoHash", infoHash),
			zap.Binary("name", []byte(name)),
		)

		return nil
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return errors.Wrap(err, "conn.Begin")
	}
	// If everything goes as planned and no error occurs, we will commit the transaction before
	// returning from the function so the tx.Rollback() call will fail, trying to rollback a
	// committed transaction. BUT, if an error occurs, we'll get our transaction rollback'ed, which
	// is nice.
	defer tx.Rollback()

	var totalSize uint64 = 0
	for _, file := range files {
		totalSize += uint64(file.Size)
	}

	// This is a workaround for a bug: the database will not accept total_size to be zero.
	if totalSize == 0 {
		zap.L().Debug("Ignoring a torrent whose total size is zero.")
		return nil
	}

	if exist, err := db.DoesTorrentExist(infoHash); exist || err != nil {
		return err
	}

	var lastInsertId int64

	err = tx.QueryRow(`
		INSERT INTO torrents (
			origin_id,
			info_hash,
			name,
			total_size,
			discovered_on
		) VALUES ($1, $2, $3, $4, $5)
		RETURNING id;
	`, db.origin_id, infoHash, name, totalSize, time.Now().Unix()).Scan(&lastInsertId)
	if err != nil {
		return errors.Wrap(err, "tx.QueryRow (INSERT INTO torrents)")
	}

	for _, file := range files {
		if !utf8.ValidString(file.Path) {
			zap.L().Warn(
				"Ignoring a file whose path is not UTF-8 compliant.",
				zap.Binary("path", []byte(file.Path)),
			)

			// Returning nil so deferred tx.Rollback() will be called and transaction will be canceled.
			return nil
		}

		_, err = tx.Exec("INSERT INTO files (torrent_id, size, path) VALUES ($1, $2, $3);",
			lastInsertId, file.Size, file.Path,
		)
		if err != nil {
			return errors.Wrap(err, "tx.Exec (INSERT INTO files)")
		}
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "tx.Commit")
	}

	return nil
}

func (db *postgresDatabase) Close() error {
	return db.conn.Close()
}

func (db *postgresDatabase) GetNumberOfTorrents() (uint, error) {
	// Using estimated number of rows which can make queries much faster
	// https://www.postgresql.org/message-id/568BF820.9060101%40comarch.com
	// https://wiki.postgresql.org/wiki/Count_estimate
	rows, err := db.conn.Query(
		"SELECT GREATEST(reltuples::BIGINT,0) AS estimate_count FROM pg_class WHERE relname='torrents';",
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	if !rows.Next() {
		return 0, fmt.Errorf("no rows returned from `SELECT reltuples::BIGINT AS estimate_count`")
	}

	// Returns int64: https://godoc.org/github.com/lib/pq#hdr-Data_Types
	var n *uint
	if err = rows.Scan(&n); err != nil {
		return 0, err
	}

	// If the database is empty (i.e. 0 entries in 'torrents') then the query will return nil.
	if n == nil {
		return 0, nil
	} else {
		return *n, nil
	}
}

func (db *postgresDatabase) QueryTorrents(
	query string,
	epoch int64,
	orderBy OrderingCriteria,
	ascending bool,
	limit uint,
	lastOrderedValue *float64,
	lastID *uint64,
) ([]TorrentMetadata, error) {
	if query == "" && orderBy == ByRelevance {
		return nil, fmt.Errorf("torrents cannot be ordered by relevance when the query is empty")
	}
	if (lastOrderedValue == nil) != (lastID == nil) {
		return nil, fmt.Errorf("lastOrderedValue and lastID should be supplied together, if supplied")
	}

	doJoin := query != ""
	firstPage := lastID == nil

	queryArgs := make([]interface{}, 0)
	var sqlQuery string

	// executeTemplate is used to prepare the SQL query, WITH PLACEHOLDERS FOR USER INPUT.
	if doJoin {
		queryArgs = append(queryArgs, query)
		queryArgs = append(queryArgs, epoch)
		if firstPage {
			queryArgs = append(queryArgs, limit)
			sqlQuery = executeTemplate(`
					SELECT id
									, info_hash
						, name
						, total_size
						, discovered_on
						, (SELECT COUNT(*) FROM files WHERE torrents.id = files.torrent_id) AS n_files
						, idx.rank
					FROM torrents
					INNER JOIN (
						SELECT id
							, pgroonga_score(tableoid, ctid) AS rank
						FROM torrents
						WHERE name &@~ $1::text
					) AS idx USING(id)
					WHERE     discovered_on <= $2::integer
					ORDER BY {{.OrderOn}} {{AscOrDesc .Ascending}}, id {{AscOrDesc .Ascending}}
					LIMIT $3::integer;
				`, struct {
				DoJoin    bool
				FirstPage bool
				OrderOn   string
				Ascending bool
			}{
				DoJoin:    doJoin,
				FirstPage: firstPage,
				OrderOn:   orderOn(orderBy),
				Ascending: ascending,
			}, template.FuncMap{
				"GTEorLTE": func(ascending bool) string {
					if ascending {
						return ">"
					} else {
						return "<"
					}
				},
				"AscOrDesc": func(ascending bool) string {
					if ascending {
						return "ASC"
					} else {
						return "DESC"
					}
				},
			})
		} else {
			queryArgs = append(queryArgs, lastOrderedValue)
			queryArgs = append(queryArgs, lastID)
			queryArgs = append(queryArgs, limit)
			sqlQuery = executeTemplate(`
			SELECT id
							 , info_hash
				 , name
				 , total_size
				 , discovered_on
				 , (SELECT COUNT(*) FROM files WHERE torrents.id = files.torrent_id) AS n_files
				 , idx.rank
			FROM torrents
			INNER JOIN (
				SELECT id
					 , pgroonga_score(tableoid, ctid) AS rank
				FROM torrents
				WHERE name &@~ $1::text
			) AS idx USING(id)
			WHERE     discovered_on <= $2::integer
					AND ( {{.OrderOn}}, id ) {{GTEorLTE .Ascending}} ($3::integer, $4::integer) -- https://www.sqlite.org/rowvalue.html#row_value_comparisons
			ORDER BY {{.OrderOn}} {{AscOrDesc .Ascending}}, id {{AscOrDesc .Ascending}}
			LIMIT $5::integer;
		`, struct {
				DoJoin    bool
				FirstPage bool
				OrderOn   string
				Ascending bool
			}{
				DoJoin:    doJoin,
				FirstPage: firstPage,
				OrderOn:   orderOn(orderBy),
				Ascending: ascending,
			}, template.FuncMap{
				"GTEorLTE": func(ascending bool) string {
					if ascending {
						return ">"
					} else {
						return "<"
					}
				},
				"AscOrDesc": func(ascending bool) string {
					if ascending {
						return "ASC"
					} else {
						return "DESC"
					}
				},
			})
		}
	} else {
		queryArgs = append(queryArgs, epoch)
		if firstPage {
			queryArgs = append(queryArgs, limit)
			sqlQuery = executeTemplate(`
			SELECT id
							 , info_hash
				 , name
				 , total_size
				 , discovered_on
				 , (SELECT COUNT(*) FROM files WHERE torrents.id = files.torrent_id) AS n_files
				 , 0
			FROM torrents
			WHERE     discovered_on <= $1::integer
			ORDER BY {{.OrderOn}} {{AscOrDesc .Ascending}}, id {{AscOrDesc .Ascending}}
			LIMIT $2::integer;
		`, struct {
				DoJoin    bool
				FirstPage bool
				OrderOn   string
				Ascending bool
			}{
				DoJoin:    doJoin,
				FirstPage: firstPage,
				OrderOn:   orderOn(orderBy),
				Ascending: ascending,
			}, template.FuncMap{
				"GTEorLTE": func(ascending bool) string {
					if ascending {
						return ">"
					} else {
						return "<"
					}
				},
				"AscOrDesc": func(ascending bool) string {
					if ascending {
						return "ASC"
					} else {
						return "DESC"
					}
				},
			})
		} else {
			queryArgs = append(queryArgs, lastOrderedValue)
			queryArgs = append(queryArgs, lastID)
			queryArgs = append(queryArgs, limit)
			sqlQuery = executeTemplate(`
			SELECT id
							 , info_hash
				 , name
				 , total_size
				 , discovered_on
				 , (SELECT COUNT(*) FROM files WHERE torrents.id = files.torrent_id) AS n_files
				 , 0
			FROM torrents
			WHERE     discovered_on <= $1::integer
					AND ( {{.OrderOn}}, id ) {{GTEorLTE .Ascending}} ($2::integer, $3::integer) -- https://www.sqlite.org/rowvalue.html#row_value_comparisons
			ORDER BY {{.OrderOn}} {{AscOrDesc .Ascending}}, id {{AscOrDesc .Ascending}}
			LIMIT $4::integer;
		`, struct {
				DoJoin    bool
				FirstPage bool
				OrderOn   string
				Ascending bool
			}{
				DoJoin:    doJoin,
				FirstPage: firstPage,
				OrderOn:   orderOn(orderBy),
				Ascending: ascending,
			}, template.FuncMap{
				"GTEorLTE": func(ascending bool) string {
					if ascending {
						return ">"
					} else {
						return "<"
					}
				},
				"AscOrDesc": func(ascending bool) string {
					if ascending {
						return "ASC"
					} else {
						return "DESC"
					}
				},
			})
		}
	}

	rows, err := db.conn.Query(sqlQuery, queryArgs...)
	if err != nil {
		return nil, errors.Wrap(err, "query error")
	}

	torrents := make([]TorrentMetadata, 0)
	for rows.Next() {
		var torrent TorrentMetadata
		err = rows.Scan(
			&torrent.ID,
			&torrent.InfoHash,
			&torrent.Name,
			&torrent.Size,
			&torrent.DiscoveredOn,
			&torrent.NFiles,
			&torrent.Relevance,
		)
		if err != nil {
			return nil, err
		}
		torrents = append(torrents, torrent)
	}
	defer db.closeRows(rows)

	return torrents, nil
}

func (db *postgresDatabase) GetTorrent(infoHash []byte) (*TorrentMetadata, error) {
	rows, err := db.conn.Query(`
		SELECT
			t.info_hash,
			t.name,
			t.total_size,
			t.discovered_on,
			(SELECT COUNT(*) FROM files f WHERE f.torrent_id = t.id) AS n_files
		FROM torrents t
		WHERE t.info_hash = $1;`,
		infoHash,
	)
	defer db.closeRows(rows)
	if err != nil {
		return nil, err
	}

	if !rows.Next() {
		return nil, nil
	}

	var tm TorrentMetadata
	if err = rows.Scan(&tm.InfoHash, &tm.Name, &tm.Size, &tm.DiscoveredOn, &tm.NFiles); err != nil {
		return nil, err
	}

	return &tm, nil
}

func (db *postgresDatabase) GetFiles(infoHash []byte) ([]File, error) {
	rows, err := db.conn.Query(`
		SELECT
       		f.size,
       		f.path
		FROM files f, torrents t WHERE f.torrent_id = t.id AND t.info_hash = $1;`,
		infoHash,
	)
	defer db.closeRows(rows)
	if err != nil {
		return nil, err
	}

	var files []File
	for rows.Next() {
		var file File
		if err = rows.Scan(&file.Size, &file.Path); err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	return files, nil
}

func (db *postgresDatabase) GetStatistics(from string, n uint) (*Statistics, error) {
	fromTime, gran, err := ParseISO8601(from)
	if err != nil {
		return nil, errors.Wrap(err, "parsing ISO8601 error")
	}

	var toTime time.Time
	var timef string // time format: https://www.sqlite.org/lang_datefunc.html

	switch gran {
	case Year:
		toTime = fromTime.AddDate(int(n), 0, 0)
		timef = "YYYY"
	case Month:
		toTime = fromTime.AddDate(0, int(n), 0)
		timef = "YYYY-MM"
	case Week:
		toTime = fromTime.AddDate(0, 0, int(n)*7)
		timef = "YYYY-IW"
	case Day:
		toTime = fromTime.AddDate(0, 0, int(n))
		timef = "YYYY-MM-DD"
	case Hour:
		toTime = fromTime.Add(time.Duration(n) * time.Hour)
		timef = `YYYY-MM-DD"T"HH24`
	}

	// TODO: make it faster!
	rows, err := db.conn.Query(fmt.Sprintf(`
			SELECT to_char(to_timestamp(discovered_on),'%s') AS dT
				, sum(files.size) AS tS
				, count(DISTINCT torrents.id) AS nD
				, count(DISTINCT files.id) AS nF
			FROM torrents, files
			WHERE torrents.id = files.torrent_id
				AND discovered_on >= $1::integer
				AND discovered_on <= $2::integer
			GROUP BY dt;`,
		timef),
		fromTime.Unix(), toTime.Unix())
	if err != nil {
		return nil, err
	}

	stats := NewStatistics()

	for rows.Next() {
		var dT string
		var tS, nD, nF uint64
		if err := rows.Scan(&dT, &tS, &nD, &nF); err != nil {
			if err := rows.Close(); err != nil {
				panic(err.Error())
			}
			return nil, err
		}
		stats.NDiscovered[dT] = nD
		stats.TotalSize[dT] = tS
		stats.NFiles[dT] = nF
	}
	defer closeRows(rows)

	return stats, nil
}

func (db *postgresDatabase) setupDatabase() error {
	// Create extensions
	ext_tx, err := db.conn.Begin()
	if err != nil {
		return errors.Wrap(err, "sql.DB.Begin")
	}
	defer ext_tx.Rollback()
	ext_tx.Exec("CREATE EXTENSION pg_trgm;")
	ext_tx.Exec("CREATE EXTENSION pgroonga;")
	ext_tx.Commit() // ignore any errors just do the thing

	tx, err := db.conn.Begin()
	if err != nil {
		return errors.Wrap(err, "sql.DB.Begin")
	}

	defer tx.Rollback()

	rows, err := db.conn.Query("SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm';")
	if err != nil {
		return err
	}
	defer db.closeRows(rows)

	trgmInstalled := rows.Next()
	if rows.Err() != nil {
		return err
	}

	if !trgmInstalled {
		return fmt.Errorf(
			"pg_trgm extension is not enabled. You need to execute 'CREATE EXTENSION pg_trgm' on this database",
		)
	}

	// Initial Setup for schema version 0:
	// FROZEN.
	_, err = tx.Exec(`
		CREATE SCHEMA IF NOT EXISTS ` + db.schema + `;

		-- Torrents ID sequence generator
		CREATE SEQUENCE IF NOT EXISTS seq_torrents_id;
		-- Files ID sequence generator
		CREATE SEQUENCE IF NOT EXISTS seq_files_id;

		CREATE TABLE IF NOT EXISTS torrents (
			id             INTEGER PRIMARY KEY DEFAULT nextval('seq_torrents_id'),
			origin_id      INTEGER NOT NULL,
			info_hash      bytea NOT NULL UNIQUE,
			name           TEXT NOT NULL,
			total_size     BIGINT NOT NULL CHECK(total_size > 0),
			discovered_on  INTEGER NOT NULL CHECK(discovered_on > 0)
		);

		-- Indexes for search sorting options
		CREATE INDEX IF NOT EXISTS idx_torrents_total_size ON torrents (total_size);
		CREATE INDEX IF NOT EXISTS idx_torrents_discovered_on ON torrents (discovered_on);

		-- Using pg_trgm GIN index for fast ILIKE queries
		-- You need to execute "CREATE EXTENSION pg_trgm" on your database for this index to work
		-- Be aware that using this type of index implies that making ILIKE queries with less that
		-- 3 character values will cause full table scan instead of using index.
		-- You can try to avoid that by doing 'SET enable_seqscan=off'.
		CREATE INDEX IF NOT EXISTS idx_torrents_name_gin_trgm ON torrents USING GIN (name gin_trgm_ops);

		CREATE TABLE IF NOT EXISTS files (
			id          INTEGER PRIMARY KEY DEFAULT nextval('seq_files_id'),
			torrent_id  INTEGER REFERENCES torrents ON DELETE CASCADE ON UPDATE RESTRICT,
			size        BIGINT NOT NULL,
			path        TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_files_torrent_id ON files (torrent_id);
		CREATE INDEX IF NOT EXISTS pgroonga_torrents_name_index ON torrents USING pgroonga (name);

		CREATE TABLE IF NOT EXISTS migrations (
		    schema_version		SMALLINT NOT NULL UNIQUE
		);

		INSERT INTO migrations (schema_version) VALUES (0) ON CONFLICT DO NOTHING;
	`)
	if err != nil {
		return errors.Wrap(err, "sql.Tx.Exec (v0)")
	}

	// Get current schema version
	rows, err = tx.Query("SELECT MAX(schema_version) FROM migrations;")
	if err != nil {
		return errors.Wrap(err, "sql.Tx.Query (SELECT MAX(version) FROM migrations)")
	}
	defer db.closeRows(rows)

	var schemaVersion int
	if !rows.Next() {
		return fmt.Errorf("sql.Rows.Next (SELECT MAX(version) FROM migrations): Query did not return any rows")
	}
	if err = rows.Scan(&schemaVersion); err != nil {
		return errors.Wrap(err, "sql.Rows.Scan (MAX(version))")
	}
	// If next line is removed we're getting error on sql.Tx.Commit: unexpected command tag SELECT
	// https://stackoverflow.com/questions/36295883/golang-postgres-commit-unknown-command-error/36866993#36866993
	db.closeRows(rows)

	// Uncomment for future migrations:
	//switch schemaVersion {
	//case 0: // FROZEN.
	//	zap.L().Warn("Updating (fake) database schema from 0 to 1...")
	//	_, err = tx.Exec(`INSERT INTO migrations (schema_version) VALUES (1);`)
	//	if err != nil {
	//		return errors.Wrap(err, "sql.Tx.Exec (v0 -> v1)")
	//	}
	//	//fallthrough
	//}

	if err = tx.Commit(); err != nil {
		return errors.Wrap(err, "sql.Tx.Commit")
	}

	return nil
}

func (db *postgresDatabase) closeRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		zap.L().Error("could not close row", zap.Error(err))
	}
}
