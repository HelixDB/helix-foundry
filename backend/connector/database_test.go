package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The test databases come from compose.test.yaml (docker compose -f compose.test.yaml up -d --wait).
const (
	testMySQLAddr = "127.0.0.1:3307"
	// testMySQLGTIDAddr runs with gtid_mode ON, for the binlog reader's GTID positions.
	testMySQLGTIDAddr = "127.0.0.1:3309"
	testPostgresHost  = "127.0.0.1"
	testPostgresPort  = 5433
)

const customersAndOrders = `
CREATE TABLE customers (id INT PRIMARY KEY, email VARCHAR(200) NOT NULL);
CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT NULL, total DECIMAL(10,2),
  FOREIGN KEY (customer_id) REFERENCES customers (id));
CREATE TABLE notes (body TEXT);`

// mysqlFixture creates a database holding customersAndOrders and returns its name.
func mysqlFixture(t *testing.T) string {
	t.Helper()
	root, err := sql.Open("mysql", "root:helix@tcp("+testMySQLAddr+")/?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	name := fmt.Sprintf("connector_test_%d", time.Now().UnixNano())
	if _, err := root.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create test database (are the test databases up? compose.test.yaml): %v", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE " + name) })
	if _, err := root.Exec("USE " + name + ";" + customersAndOrders); err != nil {
		t.Fatal(err)
	}
	return name
}

// postgresFixture creates a schema holding customersAndOrders and returns its name.
func postgresFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://helix:helix@%s:%d/helix_connector_test?sslmode=disable", testPostgresHost, testPostgresPort))
	if err != nil {
		t.Fatalf("connect test postgres (are the test databases up? compose.test.yaml): %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	name := fmt.Sprintf("connector_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+name+"; SET search_path TO "+name+";"+customersAndOrders); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE") })
	return name
}

func seedMySQL(t *testing.T, database, statements string) {
	t.Helper()
	db, err := sql.Open("mysql", "root:helix@tcp("+testMySQLAddr+")/"+database+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statements); err != nil {
		t.Fatal(err)
	}
}

func seedPostgres(t *testing.T, schema, statements string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://helix:helix@%s:%d/helix_connector_test?sslmode=disable", testPostgresHost, testPostgresPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+";"+statements); err != nil {
		t.Fatal(err)
	}
}

func discoverDatabase(t *testing.T, kind Kind, settings map[string]any, password string) (Discovery, error) {
	t.Helper()
	raw, _ := json.Marshal(settings)
	prepared, err := kind.Prepare(Source{Settings: raw, Secret: password})
	if err != nil {
		return Discovery{}, err
	}
	return kind.Discover(context.Background(), Source{Settings: prepared, Secret: password})
}

// assertCustomersAndOrders checks what both databases must report for the fixture.
func assertCustomersAndOrders(t *testing.T, d Discovery) {
	t.Helper()
	if names := tableNames(d); !slices.Equal(names, []string{"customers", "notes", "orders"}) {
		t.Fatalf("tables = %v", names)
	}
	orders := findTable(t, d, "orders")
	if !slices.Equal(orders.PrimaryKey, []string{"id"}) {
		t.Fatalf("orders primary key = %v", orders.PrimaryKey)
	}
	if !slices.Equal(orders.ForeignKeys, []ForeignKey{{Column: "customer_id", ReferencedTable: "customers", ReferencedColumn: "id"}}) {
		t.Fatalf("orders foreign keys = %+v", orders.ForeignKeys)
	}
	if len(orders.Columns) != 3 || orders.Columns[1].Name != "customer_id" || !orders.Columns[1].Nullable ||
		!strings.Contains(orders.Columns[2].Type, "numeric") && !strings.Contains(orders.Columns[2].Type, "decimal") {
		t.Fatalf("orders columns = %+v", orders.Columns)
	}
	if notes := findTable(t, d, "notes"); len(notes.PrimaryKey) != 0 {
		t.Fatalf("notes primary key = %v", notes.PrimaryKey)
	}
}

// everyCapture is a plan that allows every capture method.
var everyCapture = []Capture{CaptureFull, CaptureCursor, CaptureEvents, CaptureLog}

func TestMySQLConnector(t *testing.T) {
	local := NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
	base := func(database string) map[string]any {
		return map[string]any{"host": "127.0.0.1", "port": 3307, "database": database, "user": "root", "disable_tls": true}
	}

	t.Run("reports tables, keys and columns", func(t *testing.T) {
		d, err := discoverDatabase(t, kindOf(t, local, "mysql"), base(mysqlFixture(t)), "helix")
		if err != nil {
			t.Fatal(err)
		}
		assertCustomersAndOrders(t, d)
	})
	// A chosen table must exist, so a typo fails at setup rather than at the first sync.
	t.Run("rejects a chosen table the database lacks", func(t *testing.T) {
		settings := base(mysqlFixture(t))
		settings["tables"] = []string{"orders", "invoices"}
		_, err := discoverDatabase(t, kindOf(t, local, "mysql"), settings, "helix")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, `"invoices"`) {
			t.Fatalf("Discover error = %v, want the missing table named", err)
		}
	})
	t.Run("reports a wrong password as an authentication failure", func(t *testing.T) {
		_, err := discoverDatabase(t, kindOf(t, local, "mysql"), base("helix_provision_test"), "wrong")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.HasPrefix(refusedErr.Message, "authentication failed") {
			t.Fatalf("Discover error = %v, want an authentication failure", err)
		}
	})
	// On a hosted deployment the guard is on, so a customer cannot point a connector at our network.
	t.Run("refuses a database inside our network", func(t *testing.T) {
		catalog := NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{})
		_, err := discoverDatabase(t, kindOf(t, catalog, "mysql"), base("helix_provision_test"), "helix")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Discover error = %v, want InvalidError", err)
		}
	})
}

// postgresReader creates a login role that may only read schema and returns its name; its password is "reader".
func postgresReader(t *testing.T, schema string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://helix:helix@%s:%d/helix_connector_test?sslmode=disable", testPostgresHost, testPostgresPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	role := "reader_" + strings.TrimPrefix(schema, "connector_test_")
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE ROLE %[1]s LOGIN PASSWORD 'reader';
GRANT USAGE ON SCHEMA %[2]s TO %[1]s; GRANT SELECT ON ALL TABLES IN SCHEMA %[2]s TO %[1]s`, role, schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), fmt.Sprintf("DROP OWNED BY %[1]s; DROP ROLE %[1]s", role))
	})
	return role
}

func TestPostgresConnector(t *testing.T) {
	local := NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
	base := func(schema string) map[string]any {
		return map[string]any{
			"host": testPostgresHost, "port": testPostgresPort, "database": "helix_connector_test",
			"user": "helix", "schema": schema, "disable_tls": true,
		}
	}

	t.Run("reports tables, keys and columns of the schema", func(t *testing.T) {
		d, err := discoverDatabase(t, kindOf(t, local, "postgres"), base(postgresFixture(t)), "helix")
		if err != nil {
			t.Fatal(err)
		}
		assertCustomersAndOrders(t, d)
	})
	// information_schema hides constraints from a user with only SELECT; customers connect such users.
	t.Run("reports keys to a user that can only read", func(t *testing.T) {
		schema := postgresFixture(t)
		settings := base(schema)
		settings["user"] = postgresReader(t, schema)
		d, err := discoverDatabase(t, kindOf(t, local, "postgres"), settings, "reader")
		if err != nil {
			t.Fatal(err)
		}
		assertCustomersAndOrders(t, d)
	})
	// TLS is on unless turned off; a server without it must fail, never fall back to plaintext.
	t.Run("does not fall back to plaintext when tls is on", func(t *testing.T) {
		settings := base(postgresFixture(t))
		delete(settings, "disable_tls")
		_, err := discoverDatabase(t, kindOf(t, local, "postgres"), settings, "helix")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) {
			t.Fatalf("Discover error = %v, want RefusedError", err)
		}
	})
	t.Run("refuses a database inside our network", func(t *testing.T) {
		catalog := NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{})
		_, err := discoverDatabase(t, kindOf(t, catalog, "postgres"), base("public"), "helix")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Discover error = %v, want InvalidError", err)
		}
	})
}
