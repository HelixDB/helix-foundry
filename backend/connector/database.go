package connector

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// DatabaseSettings are a Postgres or MySQL source's connection settings; the
// password is the connector's secret.
type DatabaseSettings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	// Schema scopes a Postgres source; MySQL uses Database.
	Schema string `json:"schema,omitempty"`
	// Tables limits the sync to these tables; empty means every table.
	Tables     []string `json:"tables,omitempty"`
	DisableTLS bool     `json:"disable_tls,omitempty"`
	// CA is a PEM bundle trusted besides the system roots.
	CA string `json:"ca,omitempty"`
	// CursorColumns maps a table to the column its application updates on
	// every write; a change sync reads only rows past the column's last value.
	CursorColumns map[string]string `json:"cursor_columns,omitempty"`
	// CDC reads changes from the database's own log, on plans with log capture.
	CDC *DatabaseCDC `json:"cdc,omitempty"`
}

// DatabaseCDC is change data capture from a database's log.
type DatabaseCDC struct {
	Enabled bool `json:"enabled"`
	// Publication is the PostgreSQL publication the customer created.
	Publication string `json:"publication,omitempty"`
	// ServerID is the MySQL replica id; 0 derives one from the connector.
	ServerID uint32 `json:"server_id,omitempty"`
}

// defaultPublication is the publication PostgreSQL CDC reads when none is named.
const defaultPublication = "helix_foundry"

func prepareDatabase(src Source, defaultPort int, defaultSchema string) (DatabaseSettings, error) {
	var s DatabaseSettings
	if err := json.Unmarshal(orEmpty(src.Settings), &s); err != nil {
		return s, invalid("database settings are malformed")
	}
	s.Host, s.Database, s.User = strings.TrimSpace(s.Host), strings.TrimSpace(s.Database), strings.TrimSpace(s.User)
	switch {
	case s.Host == "" || strings.ContainsAny(s.Host, "/ \t"):
		return s, invalid("enter the database server's host name or IP address")
	case s.Database == "":
		return s, invalid("enter the database name")
	case s.User == "":
		return s, invalid("enter the database user")
	case src.Secret == "":
		return s, invalid("enter the database password")
	}
	if s.Port == 0 {
		s.Port = defaultPort
	}
	if s.Port < 1 || s.Port > 65535 {
		return s, invalid("the database port must be between 1 and 65535")
	}
	if defaultSchema != "" {
		s.Schema = strings.TrimSpace(s.Schema)
		if s.Schema == "" {
			s.Schema = defaultSchema
		}
	} else if s.Schema != "" {
		return s, invalid("this database has no schema setting; use the database name")
	}
	seen := map[string]bool{}
	tables := s.Tables[:0]
	for _, t := range s.Tables {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tables = append(tables, t)
	}
	s.Tables = tables
	cursors := map[string]string{}
	for table, column := range s.CursorColumns {
		table, column = strings.TrimSpace(table), strings.TrimSpace(column)
		if table == "" || column == "" {
			continue
		}
		if len(s.Tables) > 0 && !slices.Contains(s.Tables, table) {
			return s, invalid("cursor column %s names table %s, which is not chosen", column, table)
		}
		cursors[table] = column
	}
	s.CursorColumns = nil
	if len(cursors) > 0 {
		s.CursorColumns = cursors
	}
	if s.CDC != nil && !s.CDC.Enabled {
		s.CDC = nil
	}
	if s.CDC != nil && defaultSchema != "" {
		s.CDC.Publication = strings.TrimSpace(s.CDC.Publication)
		if s.CDC.Publication == "" {
			s.CDC.Publication = defaultPublication
		}
	}
	if s.CA != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(s.CA)) {
		return s, invalid("the CA certificate is not a PEM certificate")
	}
	return s, nil
}

// tlsConfig verifies the server against the system roots plus the settings' CA.
func (s DatabaseSettings) tlsConfig() *tls.Config {
	if s.DisableTLS {
		return nil
	}
	cfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	if s.CA != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM([]byte(s.CA))
		cfg.RootCAs = pool
	}
	return cfg
}

var (
	tlsFailureRE  = regexp.MustCompile(`(?i)certificate|tls|ssl|x509`)
	authFailureRE = regexp.MustCompile(`(?i)password|denied|auth`)
)

// connectFailure turns a driver error into a message the customer can act on;
// driver text can echo the host or user, so it stays in the wrapped error only.
func connectFailure(err error) error {
	switch {
	case errors.Is(err, ErrPrivateAddress):
		return invalid("that host is not on the public internet; Helix can only reach public database hosts")
	case tlsFailureRE.MatchString(err.Error()):
		return refused(err, "TLS verification failed; check the CA certificate and server name, then test again")
	case authFailureRE.MatchString(err.Error()):
		return refused(err, "authentication failed; check the username, password and read permissions, then test again")
	}
	return refused(err, "could not connect; check the host, port and connection settings, then test again")
}

// tableSet accumulates catalog rows into tables in name order.
type tableSet struct {
	byName map[string]*Table
}

func newTableSet() *tableSet { return &tableSet{byName: map[string]*Table{}} }

func (t *tableSet) table(name string) *Table {
	if tbl, ok := t.byName[name]; ok {
		return tbl
	}
	tbl := &Table{Name: name}
	t.byName[name] = tbl
	return tbl
}

func (t *tableSet) discovery(selected []string) (Discovery, error) {
	for _, name := range selected {
		if _, ok := t.byName[name]; !ok {
			return Discovery{}, invalid("table %q is not in this database, or the user cannot read it", name)
		}
	}
	names := make([]string, 0, len(t.byName))
	for name := range t.byName {
		names = append(names, name)
	}
	slices.Sort(names)
	var d Discovery
	for _, name := range names {
		d.Tables = append(d.Tables, *t.byName[name])
	}
	return d, nil
}
