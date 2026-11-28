package syncdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDashboardDependencyError(t *testing.T) {
	const secret = "sensitive-driver-message"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"mysql table permission", &mysql.MySQLError{Number: 1142, Message: secret}, "MySQL error 1142; SELECT permission"},
		{"mysql column permission", &mysql.MySQLError{Number: 1143, Message: secret}, "MySQL error 1143; SELECT permission"},
		{"mysql column missing", &mysql.MySQLError{Number: 1054, Message: secret}, "MySQL error 1054; expected dependency column is missing"},
		{"mysql table missing", &mysql.MySQLError{Number: 1146, Message: secret}, "MySQL error 1146; dependency table is missing"},
		{"mysql other", &mysql.MySQLError{Number: 1047, Message: secret}, "MySQL error 1047"},
		{"postgres permission", &pgconn.PgError{Code: "42501", Message: secret, Detail: secret}, "PostgreSQL SQLSTATE 42501; SELECT permission"},
		{"postgres column missing", &pgconn.PgError{Code: "42703", Message: secret}, "PostgreSQL SQLSTATE 42703; expected dependency column is missing"},
		{"postgres table missing", &pgconn.PgError{Code: "42P01", Message: secret}, "PostgreSQL SQLSTATE 42P01; dependency table is missing"},
		{"postgres other", &pgconn.PgError{Code: "XX000", Message: secret}, "PostgreSQL SQLSTATE XX000"},
		{"invalid sqlstate", &pgconn.PgError{Code: secret, Message: secret}, "database query failed"},
		{"canceled", context.Canceled, "operation canceled"},
		{"deadline", context.DeadlineExceeded, "query deadline exceeded"},
		{"unknown", errors.New(secret), "database query failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.err, fmt.Errorf("%s: %w", secret, tc.err)} {
				got := dashboardDependencyError("dashboard_provisioning", err).Error()
				if !strings.HasPrefix(got, "cannot inspect dashboard dependencies in dashboard_provisioning: ") || !strings.Contains(got, tc.want) {
					t.Fatalf("unexpected diagnostic: %s", got)
				}
				if strings.Contains(got, secret) {
					t.Fatal("diagnostic includes the raw driver error")
				}
			}
		})
	}
}
