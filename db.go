package main

import (
	"bytes"
	"database/sql"
	"encoding/gob"
	"errors"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

var ErrNotAuthorized = errors.New("Not authorized")

// ZerodropEntry is a page entry.
type ZerodropEntry struct {
	Name                 string    // The request URI used to access this entry
	URL                  string    // The URL that this entry references
	Filename             string    // The location of the file in the uploads directory
	ContentType          string    // The MIME type to serve as Content-Type header
	Redirect             bool      // Indicates whether to redirect instead of proxy
	Creation             time.Time // The time this entry was created
	AccessRedirectOnDeny string    // Entry to redirect to if entry is blacklisted or expired
	AccessBlacklist      Blacklist // Blacklist
	AccessBlacklistCount int       // Number of requests that have been caught by the blacklist
	AccessExpire         bool      // Indicates whether to expire after finite access
	AccessExpireCount    int       // The number of requests on this entry before expiry
	AccessCount          int       // The number of times this has been accessed
	AccessTrain          bool      // Whether training is active
}

// ZerodropDB represents a database connection.
// TODO: Use a persistent backend.
type ZerodropDB struct {
	*sql.DB

	// Accessors
	GetStmt       *sql.Stmt
	ListStmt      *sql.Stmt
	ListTokenStmt *sql.Stmt

	// Restricted Mutators
	UpdateCheckTokenStmt *sql.Stmt
	DeleteStmt           *sql.Stmt
	ClearStmt            *sql.Stmt

	// Admin Mutators
	AdminUpdateStmt *sql.Stmt
	AdminDeleteStmt *sql.Stmt
	AdminClearStmt  *sql.Stmt
}

// Connect opens a connection to the backend.
func (d *ZerodropDB) Connect(driver, source string) error {
	db, err := sql.Open(driver, source)
	if err != nil {
		return err
	}

	gob.Register(&ZerodropEntry{})

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS entries (
		name TEXT PRIMARY KEY NOT NULL,
		token TEXT NOT NULL,
		creation INTEGER NOT NULL,
		gob BLOB NOT NULL
	)`); err != nil {
		return err
	}

	// Accessors
	d.GetStmt, err = db.Prepare(
		`SELECT gob FROM entries WHERE name = ?`)
	if err != nil {
		return err
	}
	d.ListStmt, err = db.Prepare(
		`SELECT gob FROM entries ORDER BY creation DESC`)
	if err != nil {
		return err
	}
	d.ListTokenStmt, err = db.Prepare(
		`SELECT gob FROM entries WHERE token = ? ORDER BY creation DESC`)
	if err != nil {
		return err
	}

	// Restricted Mutators
	d.UpdateCheckTokenStmt, err = db.Prepare(
		`SELECT token FROM entries WHERE name = ?`)
	if err != nil {
		return err
	}
	d.DeleteStmt, err = db.Prepare(
		`DELETE FROM entries WHERE name = ? AND token = ?`)
	if err != nil {
		return err
	}
	d.ClearStmt, err = db.Prepare(
		`DELETE FROM entries WHERE token = ?`)
	if err != nil {
		return err
	}

	// Admin Mutators
	d.AdminUpdateStmt, err = db.Prepare(
		`REPLACE INTO entries (name, token, creation, gob) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	d.AdminDeleteStmt, err = db.Prepare(
		`DELETE FROM entries WHERE name = ?`)
	if err != nil {
		return err
	}
	d.AdminClearStmt, err = db.Prepare(
		`DELETE FROM entries`)
	if err != nil {
		return err
	}

	d.DB = db
	return nil
}

// Get returns the entry with the specified name.
func (d *ZerodropDB) Get(name string) (*ZerodropEntry, error) {
	var data []byte
	if err := d.GetStmt.QueryRow(name).Scan(&data); err != nil {
		return nil, err
	}

	var entry *ZerodropEntry
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&entry); err != nil {
		return nil, err
	}

	return entry, nil
}

// List returns a slice of all entries sorted by creation time,
// with the most recent first.
func (d *ZerodropDB) List(token string) ([]*ZerodropEntry, error) {
	list := []*ZerodropEntry{}

	var err error
	var rows *sql.Rows

	if token == "" {
		rows, err = d.ListStmt.Query()
	} else {
		rows, err = d.ListTokenStmt.Query(token)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}

		var entry *ZerodropEntry
		dec := gob.NewDecoder(bytes.NewReader(data))
		if err := dec.Decode(&entry); err != nil {
			return nil, err
		}

		list = append(list, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return list, nil
}

// Update adds an entry to the database.
func (d *ZerodropDB) Update(entry *ZerodropEntry, claims *AdminClaims) error {
	var token string

	err := d.UpdateCheckTokenStmt.QueryRow(entry.Name).Scan(&token)
	if err != nil {
		// The entry does not exist.
		token = claims.Token
	} else if !claims.Admin && token != claims.Token {
		// The entry exists and the tokens do not match.
		return ErrNotAuthorized
	}

	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	if err := enc.Encode(entry); err != nil {
		return err
	}

	if _, err := d.AdminUpdateStmt.Exec(entry.Name, token, entry.Creation.Unix(), buffer.Bytes()); err != nil {
		return err
	}

	return nil
}

// Remove removes an entry from the database with the specified token.
func (d *ZerodropDB) Remove(name string, claims *AdminClaims) error {
	if claims.Admin {
		if _, err := d.AdminDeleteStmt.Exec(name); err != nil {
			return err
		}
		return nil
	}

	if _, err := d.DeleteStmt.Exec(name, claims.Token); err != nil {
		return err
	}

	return nil
}

// Clear resets the database by removing all entries with the specified token.
func (d *ZerodropDB) Clear(claims *AdminClaims) error {
	if claims.Admin {
		if _, err := d.AdminClearStmt.Exec(); err != nil {
			return err
		}

		return nil
	}

	if _, err := d.ClearStmt.Exec(claims.Token); err != nil {
		return err
	}

	return nil
}

// IsExpired returns true if the entry is expired
func (e *ZerodropEntry) IsExpired() bool {
	return e.AccessExpire && (e.AccessCount >= e.AccessExpireCount)
}

// SetTraining sets the AccessTrain flag
func (e *ZerodropEntry) SetTraining(train bool) {
	e.AccessTrain = train
}

// Access increases the access count for an entry.
func (e *ZerodropEntry) Access() {
	e.AccessCount++
}

func (e *ZerodropEntry) String() string {
	return strconv.Quote(e.Name)
}
